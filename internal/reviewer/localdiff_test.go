package reviewer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/internal/model"
	"github.com/OpticDiff/code-reviewer/internal/vcs"
)

func numberedLines(n int, tag string) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d %s\n", i, tag)
	}
	return sb.String()
}

// repoWithChange builds a repository whose second commit changes files that
// the fake platform will not supply, and returns the two commits.
func repoWithChange(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeRepoFile(t, dir, "big.go", numberedLines(60, "old"))
	writeRepoFile(t, dir, "-dash file.go", numberedLines(10, "old"))
	writeRepoFile(t, dir, "pkg/old_name.go", numberedLines(30, "same"))
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	base = gitIn(t, dir, "rev-parse", "HEAD")

	writeRepoFile(t, dir, "big.go", strings.Replace(numberedLines(60, "old"), "line 30 old", "line 30 changed", 1))
	writeRepoFile(t, dir, "-dash file.go", numberedLines(10, "new"))
	gitIn(t, dir, "mv", "pkg/old_name.go", "pkg/new_name.go")
	writeRepoFile(t, dir, "pkg/new_name.go", numberedLines(30, "same")+"extra line\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "head")
	head = gitIn(t, dir, "rev-parse", "HEAD")
	return dir, base, head
}

func localDiffCfg() *config.Config {
	return &config.Config{
		NoCache: true, CIMode: true, Model: "gemini-2.5-flash",
		ChunkStrategy: config.ChunkStrategyFail, MinSeverity: config.SeverityLow,
		CommentMode: config.CommentModeNotes, CIProjectID: "1", CIMergeRequestID: "2",
	}
}

func withheldChanges(base, head string) *vcs.MRChanges {
	return &vcs.MRChanges{
		Title:   "t",
		BaseSHA: base, HeadSHA: head,
		Changes: []vcs.DiffEntry{
			{OldPath: "big.go", NewPath: "big.go", TooLarge: true, Incomplete: true},
			{OldPath: "-dash file.go", NewPath: "-dash file.go", Collapsed: true},
			{OldPath: "pkg/old_name.go", NewPath: "pkg/new_name.go", RenamedFile: true, Incomplete: true},
		},
	}
}

func TestRun_FilesTheplatformWithheldAreDiffedLocally(t *testing.T) {
	dir, base, head := repoWithChange(t)
	t.Chdir(dir)
	cfg := localDiffCfg()
	cfg.AuditLog = filepath.Join(t.TempDir(), "audit.jsonl")
	mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
	client := &mockVCS{mrChanges: withheldChanges(base, head)}

	if _, err := New(cfg, mm, client).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"=== File: big.go ===", "line 30 changed",
		"=== File: -dash file.go ===", "line 1 new",
		"=== File: pkg/new_name.go ===", "extra line",
	} {
		if !strings.Contains(mm.lastUserPrompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	// The line numbers are those of the real file.
	if !strings.Contains(mm.lastUserPrompt, "  30 + line 30 changed") {
		t.Errorf("changed line not numbered 30:\n%s", mm.lastUserPrompt)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) != 0 || len(e.FilesReviewed) != 3 {
		t.Errorf("reviewed = %v, skipped = %v; want all three files reviewed", e.FilesReviewed, e.FilesSkippedDetail)
	}
}

func TestRun_LocalDiffFallsBackToSkippingWhenUnavailable(t *testing.T) {
	dir, base, head := repoWithChange(t)
	tests := []struct {
		name    string
		prepare func(t *testing.T, cfg *config.Config, mr *vcs.MRChanges)
	}{
		{"setting off", func(t *testing.T, cfg *config.Config, mr *vcs.MRChanges) { cfg.DisableLocalDiff = true }},
		{"head commit not in the checkout", func(t *testing.T, cfg *config.Config, mr *vcs.MRChanges) {
			mr.HeadSHA = strings.Repeat("a", 40)
		}},
		{"platform gave no commits", func(t *testing.T, cfg *config.Config, mr *vcs.MRChanges) {
			mr.BaseSHA, mr.HeadSHA = "", ""
		}},
		{"commit that looks like a flag", func(t *testing.T, cfg *config.Config, mr *vcs.MRChanges) {
			mr.BaseSHA = "--output=/tmp/x"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(dir)
			cfg := localDiffCfg()
			cfg.AuditLog = filepath.Join(t.TempDir(), "audit.jsonl")
			mr := withheldChanges(base, head)
			tt.prepare(t, cfg, mr)
			mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
			// One file the platform did supply, so the review has something to send.
			mr.Changes = append(mr.Changes, vcs.DiffEntry{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,1 +1,2 @@\n a\n+b\n"})
			if _, err := New(cfg, mm, &mockVCS{mrChanges: mr}).Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if strings.Contains(mm.lastUserPrompt, "line 30 changed") {
				t.Error("a withheld file was diffed locally")
			}
			e := readAudit(t, cfg.AuditLog)
			if len(e.FilesSkippedDetail) != 3 {
				t.Errorf("skipped = %v, want the three withheld files as before", e.FilesSkippedDetail)
			}
		})
	}
}

func TestRun_LocalDiffLeavesExcludedFilesAlone(t *testing.T) {
	dir, base, head := repoWithChange(t)
	t.Chdir(dir)
	cfg := localDiffCfg()
	cfg.ExcludedPatterns = []string{"big.go"}
	mm := &mockModel{result: &model.ReviewResult{Summary: "ok"}}
	if _, err := New(cfg, mm, &mockVCS{mrChanges: withheldChanges(base, head)}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(mm.lastUserPrompt, "=== File: big.go") {
		t.Error("an excluded file was diffed locally")
	}
}

func TestLocalDiffer_PathsAreArgumentsNotShellText(t *testing.T) {
	dir, base, head := repoWithChange(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	l := newLocalDiffer(context.Background(), dir, base, head)
	if l == nil {
		t.Fatal("no local differ for a checkout holding both commits")
	}
	for _, name := range []string{"big.go; touch " + marker, "$(touch " + marker + ")", "--output=" + marker} {
		if patch, ok := l.patch(context.Background(), name, name); ok {
			t.Errorf("patch for %q = %q, want none", name, patch)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a path was run as a command or option")
	}
	patch, ok := l.patch(context.Background(), "big.go", "big.go")
	if !ok || !strings.HasPrefix(patch, "@@") {
		t.Fatalf("patch = %q, %v; want hunks starting at the first hunk header", patch, ok)
	}
}

func TestNewLocalDiffer_RejectsBadCommits(t *testing.T) {
	dir, base, head := repoWithChange(t)
	for _, tt := range []struct{ name, base, head string }{
		{"empty", "", head}, {"flag", "-p", head}, {"unknown", strings.Repeat("0", 40), head},
		{"not hex", "HEAD~1", head}, {"missing head", base, strings.Repeat("f", 40)},
	} {
		if l := newLocalDiffer(context.Background(), dir, tt.base, tt.head); l != nil {
			t.Errorf("%s: got a differ", tt.name)
		}
	}
	if l := newLocalDiffer(context.Background(), "", base, head); l != nil {
		t.Error("got a differ without a repository")
	}
	if l := newLocalDiffer(context.Background(), t.TempDir(), base, head); l != nil {
		t.Error("got a differ outside a repository")
	}
}
