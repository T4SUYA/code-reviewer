package reviewer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/cache"
	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

// hunkedFile returns a file of n hunks of about `tokens` estimated tokens each.
// Hunk k (1-based) starts at line 100*(k-1)+1, its added line is the next one
// and carries the text "hunk:k".
func hunkedFile(path string, n, tokens int) diff.FileDiff {
	d := diff.FileDiff{OldPath: path, NewPath: path}
	for k := 1; k <= n; k++ {
		start := 100*(k-1) + 1
		d.Hunks = append(d.Hunks, diff.Hunk{
			Header:   fmt.Sprintf("@@ -%d,1 +%d,2 @@", start, start),
			OldStart: start, OldCount: 1, NewStart: start, NewCount: 2,
			Lines: []diff.DiffLine{
				{Type: diff.LineContext, Content: "context", OldLineNo: start, NewLineNo: start},
				{Type: diff.LineAdded, Content: fmt.Sprintf("hunk:%d ", k) + strings.Repeat("x", tokens*4), NewLineNo: start + 1},
			},
		})
	}
	return d
}

func TestBuildNumberedDiff_NamesThePartOfAFile(t *testing.T) {
	d := hunkedFile("big.go", 6, 100)
	parts := diff.SplitFile(d, 250)
	out := buildNumberedDiff(parts[1:2])
	if !strings.Contains(out, "=== File: big.go (part 2 of 3) ===") {
		t.Errorf("part header missing:\n%s", out[:200])
	}
	if !strings.Contains(out, " 302 + hunk:4") || strings.Contains(out, "  2 + hunk") {
		t.Errorf("lines are not numbered as in the real file:\n%s", out[:400])
	}
	whole := buildNumberedDiff([]diff.FileDiff{d})
	if strings.Contains(whole, "part") {
		t.Error("a whole file is labelled as a part")
	}
}

// hunksIn returns the hunk numbers whose marker is in the prompt.
func hunksIn(prompt string) []int {
	var in []int
	for k := 1; k <= 9; k++ {
		if strings.Contains(prompt, fmt.Sprintf("hunk:%d ", k)) {
			in = append(in, k)
		}
	}
	return in
}

// A file larger than a chunk is reviewed in parts; each part's findings sit on
// the real file's lines and all of them are kept.
func TestRun_OversizedFileIsReviewedInParts(t *testing.T) {
	cfg := chunkCfg(t) // Chunks of at most 1280 tokens.
	var prompts []string
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		prompts = append(prompts, prompt)
		res := &model.ReviewResult{}
		for _, k := range hunksIn(prompt) {
			f := findingFor("big.go")
			f.Line = 100*(k-1) + 2
			f.Title = fmt.Sprintf("problem in hunk %d", k)
			res.Findings = append(res.Findings, f)
		}
		return res, nil
	}}
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	count, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: []diff.FileDiff{hunkedFile("big.go", 4, 500)}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one per part)", m.calls)
	}
	if !strings.Contains(prompts[0], "part 1 of 2") || !strings.Contains(prompts[1], "part 2 of 2") {
		t.Errorf("parts are not labelled in the prompts")
	}
	if count != 4 {
		t.Errorf("findings = %d, want one per hunk (4)", count)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) != 0 || len(e.FilesReviewed) != 1 {
		t.Errorf("reviewed = %v, skipped = %v; want the one file reviewed", e.FilesReviewed, e.FilesSkippedDetail)
	}
}

// When one part cannot be reviewed even in single hunks, the file is reported
// once as not reviewed and the findings of the other parts stay.
func TestRun_FailingPartIsReportedOnceAndOtherPartsKept(t *testing.T) {
	cfg := chunkCfg(t)
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := hunksIn(prompt)
		res := &model.ReviewResult{}
		for _, k := range in {
			if k == 3 {
				return nil, errors.New("unreadable answer")
			}
			f := findingFor("big.go")
			f.Line = 100*(k-1) + 2
			res.Findings = append(res.Findings, f)
		}
		return res, nil
	}}
	count, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: []diff.FileDiff{hunkedFile("big.go", 4, 500)}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Part 1 (hunks 1, 2) is fine. Part 2 fails; its halves are hunk 3 (fails
	// for good) and hunk 4 (fine).
	if count != 3 {
		t.Errorf("findings = %d, want 3 (hunks 1, 2 and 4)", count)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) != 1 || e.FilesSkippedDetail[0] != (SkippedFile{Path: "big.go", Reason: SkipModelError}) {
		t.Errorf("skipped = %v, want big.go once as model_error", e.FilesSkippedDetail)
	}
}

// A file with a part that was not reviewed is not stored in the cache as
// reviewed, and a file whose parts all were is.
func TestRun_FileWithAnUnreviewedPartIsNotCached(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failHunk   int
		wantCached bool
	}{
		{"every part reviewed", 0, true},
		{"one part not reviewed", 3, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := chunkCfg(t)
			cfg.NoCache = false
			cfg.CacheDir = t.TempDir()
			cfg.CacheMaxAge = time.Hour
			m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
				if slices.Contains(hunksIn(prompt), tt.failHunk) {
					return nil, errors.New("unreadable answer")
				}
				return &model.ReviewResult{}, nil
			}}
			d := hunkedFile("big.go", 4, 500)
			r := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: []diff.FileDiff{d}})
			if _, err := r.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			promptHash := cache.PromptHash(cfg.CustomPrompt, "", "", cfg.Focus, cfg.ExtraRules, "")
			key := cache.CacheKey(cache.DiffHash(d), reviewCacheModelID(cfg), promptHash)
			if _, ok := r.cache.Lookup(key); ok != tt.wantCached {
				t.Errorf("cached = %v, want %v", ok, tt.wantCached)
			}
		})
	}
}
