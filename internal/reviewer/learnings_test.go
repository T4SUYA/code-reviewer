package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/internal/learnings"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

func learningsRunCfg() *config.Config {
	return &config.Config{
		NoCache: true, Model: "m", Focus: []string{"all"},
		ChunkStrategy: config.ChunkStrategyFail, MinSeverity: config.SeverityLow,
	}
}

func runForPrompt(t *testing.T, cfg *config.Config, entries []learnings.Entry) *mockModel {
	t.Helper()
	mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
	r := NewWithDiffSource(cfg, mm, &mockVCS{}, &mockDiffSource{diffs: makeTestDiffs("file1.go")})
	if entries != nil {
		r.SetLearnings(entries)
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return mm
}

// Without learnings the system prompt is exactly the one built from the other
// settings, whatever else the run has loaded.
func TestRun_NoLearningsLeavesPromptUnchanged(t *testing.T) {
	cfg := learningsRunCfg()
	want := model.WithConfidenceFloor(model.BuildPromptWithProfile("", "", "", cfg.Focus, "", "", ""), cfg.ConfidenceFloor)

	for name, entries := range map[string][]learnings.Entry{
		"never set":                nil,
		"empty list":               {},
		"entries scoped elsewhere": {{Title: "t", Reason: "r", PathGlob: "docs/**"}},
	} {
		t.Run(name, func(t *testing.T) {
			mm := runForPrompt(t, learningsRunCfg(), entries)
			if mm.lastSystemPrompt != want {
				t.Errorf("system prompt differs from the baseline:\n%s", mm.lastSystemPrompt)
			}
		})
	}
}

func TestRun_LearningsReachSystemPrompt(t *testing.T) {
	mm := runForPrompt(t, learningsRunCfg(), []learnings.Entry{
		{Title: "Unchecked error", Rule: "errcheck", Reason: "Handled by the caller", Kind: learnings.KindFalsePositive},
		{Title: "Docs only", Reason: "r", PathGlob: "docs/**"},
	})
	p := mm.lastSystemPrompt
	if !strings.Contains(p, "## Previously dismissed findings in this repository") || !strings.Contains(p, `title: "Unchecked error"`) {
		t.Errorf("learnings missing from the prompt:\n%s", p)
	}
	if strings.Contains(p, "Docs only") {
		t.Error("an entry scoped to files outside the diff must not be rendered")
	}
}

// A changed learnings file re-reviews cached diffs; an unchanged or absent one
// replays them.
func TestRun_CacheReplayDependsOnLearnings(t *testing.T) {
	entryA := []learnings.Entry{{Title: "a", Reason: "r"}}
	entryB := []learnings.Entry{{Title: "b", Reason: "r"}}
	tests := []struct {
		name       string
		first, run []learnings.Entry
		wantCalls  int
	}{
		{"same learnings replay", entryA, entryA, 0},
		{"changed learnings re-review", entryA, entryB, 1},
		{"learnings added re-review", nil, entryA, 1},
		{"learnings removed re-review", entryA, nil, 1},
		{"no learnings replay", nil, nil, 0},
		{"learnings scoped elsewhere replay", nil, []learnings.Entry{{Title: "a", Reason: "r", PathGlob: "docs/**"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := learningsRunCfg()
			cfg.NoCache = false
			cfg.CacheDir = t.TempDir()
			cfg.CacheMaxAge = time.Hour

			first := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
			r1 := NewWithDiffSource(cfg, first, &mockVCS{}, &mockDiffSource{diffs: makeTestDiffs("file1.go")})
			r1.SetLearnings(tt.first)
			if _, err := r1.Run(context.Background()); err != nil || first.calls != 1 {
				t.Fatalf("first run: err=%v calls=%d, want a model call", err, first.calls)
			}

			second := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
			r2 := NewWithDiffSource(cfg, second, &mockVCS{}, &mockDiffSource{diffs: makeTestDiffs("file1.go")})
			r2.SetLearnings(tt.run)
			if _, err := r2.Run(context.Background()); err != nil {
				t.Fatalf("second run: %v", err)
			}
			if second.calls != tt.wantCalls {
				t.Errorf("model calls = %d, want %d", second.calls, tt.wantCalls)
			}
		})
	}
}

func entryTitles(entries []learnings.Entry) []string {
	var titles []string
	for _, e := range entries {
		titles = append(titles, e.Title)
	}
	return titles
}

// In CI a learnings file inside the checkout comes from the base ref, so the
// copy a contributor edits in the working tree is ignored; a file outside the
// checkout is read as given.
func TestLoadLearnings_TrustBoundary(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	file := filepath.Join(repo, "learnings.yaml")
	if err := os.WriteFile(file, []byte("- title: from-base\n  reason: r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	base := gitIn(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(file, []byte("- title: from-working-tree\n  reason: r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "learnings.yaml")
	if err := os.WriteFile(outside, []byte("- title: from-outside\n  reason: r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	tests := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"CI reads the checkout file from the base ref", config.Config{CIMode: true, CIDiffBaseSHA: base, LearningsPath: "learnings.yaml"}, []string{"from-base"}},
		{"CI reads an absolute path inside the checkout from the base ref", config.Config{CIMode: true, CIDiffBaseSHA: base, LearningsPath: file}, []string{"from-base"}},
		{"CI reads a path outside the checkout as-is", config.Config{CIMode: true, CIDiffBaseSHA: base, LearningsPath: outside}, []string{"from-outside"}},
		{"CI without a base ref ignores the checkout file", config.Config{CIMode: true, LearningsPath: "learnings.yaml"}, nil},
		{"CI file absent at the base ref has no entries", config.Config{CIMode: true, CIDiffBaseSHA: base, LearningsPath: "new-file.yaml"}, nil},
		{"outside CI reads the working tree", config.Config{LearningsPath: "learnings.yaml"}, []string{"from-working-tree"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadLearnings(context.Background(), &tt.cfg)
			if err != nil {
				t.Fatalf("LoadLearnings: %v", err)
			}
			if !reflect.DeepEqual(entryTitles(got), tt.want) {
				t.Errorf("titles = %v, want %v", entryTitles(got), tt.want)
			}
		})
	}
}

func TestLoadLearnings_BaseRefFileIsValidated(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "learnings.yaml"), []byte("- title: missing-reason\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	t.Chdir(repo)
	cfg := config.Config{CIMode: true, CIDiffBaseSHA: gitIn(t, repo, "rev-parse", "HEAD"), LearningsPath: "learnings.yaml"}
	if _, err := LoadLearnings(context.Background(), &cfg); err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("err = %v, want a validation error", err)
	}
}

func TestLoadLearnings_UnsetPathLoadsNothing(t *testing.T) {
	if got, err := LoadLearnings(context.Background(), &config.Config{}); err != nil || got != nil {
		t.Errorf("got %v, %v; want nothing", got, err)
	}
}
