package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

// markersIn returns the files whose marker is in the prompt, in file order.
func markersIn(prompt string, files []string) []string {
	var in []string
	for _, f := range files {
		if strings.Contains(prompt, "marker:"+f) {
			in = append(in, f)
		}
	}
	return in
}

// findingFor is a valid finding on the one added line of a marked diff.
func findingFor(f string) model.Finding {
	return model.Finding{File: f, Line: 1, Severity: "HIGH", Category: "bug", Title: "problem in " + f, Body: "body"}
}

// reviewsFiles answers every prompt with one finding per file in it, unless
// fail says the prompt cannot be answered.
func reviewsFiles(files []string, fail func(in []string) *model.ReviewResult, failErr error) *funcModel {
	return &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := markersIn(prompt, files)
		if fail != nil {
			if res := fail(in); res != nil {
				return res, nil
			}
		}
		if failErr != nil && fail == nil {
			return nil, failErr
		}
		res := &model.ReviewResult{Summary: "reviewed " + strings.Join(in, "+")}
		for _, f := range in {
			res.Findings = append(res.Findings, findingFor(f))
		}
		return res, nil
	}}
}

func oneChunkCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := chunkCfg(t)
	cfg.TokenLimit = 1_000_000 // Every file fits one chunk.
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	return cfg
}

func readAudit(t *testing.T, path string) AuditEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var e AuditEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &e); err != nil {
		t.Fatalf("audit log: %v", err)
	}
	return e
}

// A chunk whose call fails is reviewed again as two halves; a half that fails
// is halved in turn. Every file ends up reviewed.
func TestRun_FailedChunkIsReviewedAsHalves(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go", "d.go"}
	tests := []struct {
		name      string
		maxFiles  int // A call with more files than this fails.
		wantCalls int
		wantSplit int
	}{
		{"whole chunk fails, halves succeed", 2, 3, 1},
		{"halves fail again, single files succeed", 1, 7, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := oneChunkCfg(t)
			boom := model.MarkBadAnswer(errors.New("model returned an unusable answer"))
			m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
				in := markersIn(prompt, files)
				if len(in) > tt.maxFiles {
					return nil, boom
				}
				res := &model.ReviewResult{}
				for _, f := range in {
					res.Findings = append(res.Findings, findingFor(f))
				}
				return res, nil
			}}
			count, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if count != len(files) {
				t.Errorf("findings = %d, want one per file (%d)", count, len(files))
			}
			if m.calls != tt.wantCalls {
				t.Errorf("model calls = %d, want %d", m.calls, tt.wantCalls)
			}
			e := readAudit(t, cfg.AuditLog)
			if len(e.FilesSkippedDetail) != 0 {
				t.Errorf("skipped = %v, want none", e.FilesSkippedDetail)
			}
			if e.Chunks != 1 || e.ChunksSplit != tt.wantSplit {
				t.Errorf("chunks = %d, split = %d; want 1 and %d", e.Chunks, e.ChunksSplit, tt.wantSplit)
			}
		})
	}
}

// A file whose review keeps failing is reported as skipped; the findings of
// the other files stay and the run ends as a normal review.
func TestRun_PermanentlyFailingFileIsSkippedAndOthersSurvive(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go"}
	cfg := oneChunkCfg(t)
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := markersIn(prompt, files)
		res := &model.ReviewResult{}
		for _, f := range in {
			if f == "b.go" {
				return nil, model.MarkBadAnswer(errors.New("parsing model response: invalid character"))
			}
			res.Findings = append(res.Findings, findingFor(f))
		}
		return res, nil
	}}
	vc := &mockVCS{}
	count, err := NewWithDiffSource(cfg, m, vc, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if count != 2 {
		t.Errorf("findings = %d, want 2 (a.go and c.go)", count)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) != 1 || e.FilesSkippedDetail[0] != (SkippedFile{Path: "b.go", Reason: SkipModelError}) {
		t.Errorf("skipped = %v, want b.go model_error", e.FilesSkippedDetail)
	}
	if !strings.Contains(vc.submitReviewReq.Summary, "Reviewed 2 of 3 files. Not reviewed: 1 (model_error 1).") {
		t.Errorf("summary lacks the coverage sentence: %q", vc.submitReviewReq.Summary)
	}
}

// A response cut off by the output limit is handled like a failure: the chunk
// is halved, and a file that still gets a cut-off answer is reported.
func TestRun_TruncatedChunkIsHalvedAndFloorIsSkipped(t *testing.T) {
	files := []string{"a.go", "b.go"}
	cfg := oneChunkCfg(t)
	m := reviewsFiles(files, func(in []string) *model.ReviewResult {
		if len(in) == 2 {
			return &model.ReviewResult{Truncated: true, Findings: []model.Finding{findingFor("a.go")}}
		}
		if len(in) == 1 && in[0] == "b.go" {
			return &model.ReviewResult{Truncated: true, Findings: []model.Finding{findingFor("b.go")}}
		}
		return nil
	}, nil)
	count, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// a.go once from its own half, b.go from the partial answer kept at the floor.
	if count != 2 {
		t.Errorf("findings = %d, want 2", count)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) != 1 || e.FilesSkippedDetail[0] != (SkippedFile{Path: "b.go", Reason: SkipTruncated}) {
		t.Errorf("skipped = %v, want b.go truncated", e.FilesSkippedDetail)
	}
	if e.ChunksSplit != 1 {
		t.Errorf("chunks split = %d, want 1", e.ChunksSplit)
	}
}

// Errors that no smaller request can cure stop the run at once.
func TestRun_FatalErrorFailsTheRun(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go"}
	denied := model.MarkFatal(errors.New("credentials refused"))
	m := reviewsFiles(files, nil, denied)
	_, err := NewWithDiffSource(oneChunkCfg(t), m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if !errors.Is(err, denied) || !strings.Contains(err.Error(), "model review (chunk 1)") {
		t.Fatalf("err = %v, want the fatal error of chunk 1", err)
	}
	if m.calls != 1 {
		t.Errorf("model calls = %d, want 1: a fatal error is not retried on halves", m.calls)
	}
}

func TestRun_CancelledContextFailsTheRun(t *testing.T) {
	files := []string{"a.go", "b.go"}
	ctx, cancel := context.WithCancel(context.Background())
	m := &funcModel{fn: func(string) (*model.ReviewResult, error) {
		cancel()
		return nil, errors.New("request aborted")
	}}
	_, err := NewWithDiffSource(oneChunkCfg(t), m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(ctx)
	if err == nil {
		t.Fatal("Run succeeded after the context was cancelled")
	}
	if m.calls != 1 {
		t.Errorf("model calls = %d, want 1", m.calls)
	}
}

// When no call of the whole run ever succeeds, nothing was reviewed and the
// run fails instead of reporting every file as skipped.
func TestRun_NothingReviewedFailsTheRun(t *testing.T) {
	files := []string{"a.go", "b.go"}
	boom := errors.New("invalid argument")
	m := reviewsFiles(files, nil, boom)
	_, err := NewWithDiffSource(oneChunkCfg(t), m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the model error", err)
	}
}

// A file skipped after a failure is not reported a second time as skipped by
// the token budget.
func TestRun_ModelErrorSkipIsNotCountedAsBudgetSkip(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go"}
	cfg := chunkCfg(t) // One file per chunk.
	cfg.MaxTokens = 5000
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		if chunkOf(prompt, files) == "a.go" {
			return nil, errors.New("unusable")
		}
		return &model.ReviewResult{Usage: &model.TokenUsage{TotalTokens: 6000}}, nil
	}}
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := readAudit(t, cfg.AuditLog)
	got := map[string]string{}
	for _, s := range e.FilesSkippedDetail {
		got[s.Path] += s.Reason + ";"
	}
	want := map[string]string{"a.go": SkipModelError + ";", "c.go": SkipBudget + ";"}
	if len(got) != len(want) || got["a.go"] != want["a.go"] || got["c.go"] != want["c.go"] {
		t.Errorf("skipped = %v, want %v", got, want)
	}
}

// twoPerChunkCfg packs two of the 1000-token files into each chunk.
func twoPerChunkCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := chunkCfg(t)
	cfg.TokenLimit = 2800
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	return cfg
}

// Only an unusable answer or a request that is too large is cured by a
// smaller request. Any other error that survived the provider's retries skips
// the chunk without halving; a request the server rejects for a reason that is
// not its size fails the run.
func TestRun_OnlyErrorsASmallerRequestCuresAreHalved(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go", "d.go"}
	tests := []struct {
		name        string
		err         error
		wantCalls   int // Two chunks, plus the two halves of the first when it is halved.
		wantSkipped []string
		wantRunErr  bool
	}{
		{"unusable answer is halved", model.MarkBadAnswer(errors.New("parsing model response")), 4, []string{"a.go"}, false},
		{"413 is halved", &genai.APIError{Code: 413}, 4, []string{"a.go"}, false},
		{"400 about size is halved", &genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "input token count exceeds the maximum"}, 4, []string{"a.go"}, false},
		{"429 is not halved", &genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, 2, []string{"a.go", "b.go"}, false},
		{"quota is not halved", errors.New("generating content failed after 3 attempts: quota exceeded"), 2, []string{"a.go", "b.go"}, false},
		{"503 is not halved", &genai.APIError{Code: 503}, 2, []string{"a.go", "b.go"}, false},
		{"network error is not halved", errors.New("HTTP request failed: connection reset by peer"), 2, []string{"a.go", "b.go"}, false},
		{"timeout is not halved", fmt.Errorf("generating content: %w", context.DeadlineExceeded), 2, []string{"a.go", "b.go"}, false},
		{"400 that is not about size fails the run", &genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "thinking level is not supported"}, 1, nil, true},
		{"failed precondition fails the run", &genai.APIError{Code: 400, Status: "FAILED_PRECONDITION", Message: "location is not supported"}, 1, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := twoPerChunkCfg(t)
			m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
				in := markersIn(prompt, files)
				if slices.Contains(in, "a.go") {
					return nil, tt.err
				}
				return &model.ReviewResult{}, nil
			}}
			_, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
			if tt.wantRunErr {
				if err == nil {
					t.Fatal("Run succeeded")
				}
				if m.calls != tt.wantCalls {
					t.Errorf("calls = %d, want %d", m.calls, tt.wantCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if m.calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", m.calls, tt.wantCalls)
			}
			var got []string
			for _, s := range readAudit(t, cfg.AuditLog).FilesSkippedDetail {
				if s.Reason != SkipModelError {
					t.Errorf("%s skipped as %s, want model_error", s.Path, s.Reason)
				}
				got = append(got, s.Path)
			}
			if !slices.Equal(got, tt.wantSkipped) {
				t.Errorf("skipped = %v, want %v", got, tt.wantSkipped)
			}
		})
	}
}

// Recovery is bounded: a run in which some pieces review and many fail makes
// no more than the initial calls plus the recovery allowance.
func TestRun_RecoveryCallsAreCapped(t *testing.T) {
	var files []string
	for i := range 60 {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	cfg := oneChunkCfg(t)
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := markersIn(prompt, files)
		// A single even-numbered file reviews; everything else is unusable.
		if len(in) == 1 && (in[0][2]-'0')%2 == 0 {
			return &model.ReviewResult{}, nil
		}
		return nil, model.MarkBadAnswer(errors.New("unusable"))
	}}
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if limit := 1 + recoveryCallsFor(1); m.calls > limit {
		t.Errorf("calls = %d, want at most %d (1 initial + the recovery allowance)", m.calls, limit)
	}
	e := readAudit(t, cfg.AuditLog)
	if len(e.FilesSkippedDetail) == 0 {
		t.Error("nothing was skipped")
	}
	for _, s := range e.FilesSkippedDetail {
		if s.Reason != SkipModelError {
			t.Errorf("%s skipped as %s", s.Path, s.Reason)
		}
	}
}

// When a run has never had a complete answer, the circuit breaker fails it
// after a few dead ends instead of trying every piece.
func TestRun_BreakerFailsEarlyWhenNothingEverSucceeded(t *testing.T) {
	var files []string
	for i := range 40 {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	m := &funcModel{fn: func(string) (*model.ReviewResult, error) {
		return nil, model.MarkBadAnswer(errors.New("unusable"))
	}}
	_, err := NewWithDiffSource(oneChunkCfg(t), m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with no answer at all")
	}
	// The first chunk, its descent to a single file (about log2 of 40 steps),
	// and the dead ends that trip the breaker.
	if m.calls > 20 {
		t.Errorf("calls = %d, want the breaker to stop it early", m.calls)
	}
}

// After the breaker trips in a run that had successes, the rest is skipped
// without more calls.
func TestRun_BreakerSkipsTheRestAfterConsecutiveDeadEnds(t *testing.T) {
	var files []string
	for i := range 12 {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	cfg := chunkCfg(t) // One file per chunk.
	cfg.AuditLog = t.TempDir() + "/audit.jsonl"
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		if chunkOf(prompt, files) == "f00.go" {
			return &model.ReviewResult{}, nil
		}
		return nil, errors.New("503 unavailable")
	}}
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := 1 + breakerDeadEnds; m.calls != want {
		t.Errorf("calls = %d, want %d: the first success and the dead ends that trip the breaker", m.calls, want)
	}
	if n := len(readAudit(t, cfg.AuditLog).FilesSkippedDetail); n != 11 {
		t.Errorf("skipped = %d files, want the other 11", n)
	}
}

// Tokens of an answer that could not be used still count.
func TestRun_UsageOfAFailedAnswerIsCounted(t *testing.T) {
	files := []string{"a.go", "b.go"}
	cfg := oneChunkCfg(t)
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := markersIn(prompt, files)
		if len(in) == 2 {
			return nil, model.WithUsage(model.MarkBadAnswer(errors.New("unusable")), &model.TokenUsage{TotalTokens: 700})
		}
		return &model.ReviewResult{Usage: &model.TokenUsage{TotalTokens: 100}}, nil
	}}
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readAudit(t, cfg.AuditLog).Usage; got == nil || got.TotalTokens != 900 {
		t.Errorf("usage = %v, want 700 + 2*100", got)
	}
}

// Once the budget is spent, the remaining parts are not sent, and a truncated
// answer keeps its partial findings.
func TestRun_BudgetStopsPartsAndTruncatedAnswerKeepsItsFindings(t *testing.T) {
	files := []string{"a.go", "b.go", "c.go"}
	cfg := oneChunkCfg(t)
	cfg.MaxTokens = 20000
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		in := markersIn(prompt, files)
		if len(in) == 3 {
			return &model.ReviewResult{Truncated: true, Findings: []model.Finding{findingFor("a.go")}, Usage: &model.TokenUsage{TotalTokens: 25000}}, nil
		}
		t.Errorf("a part was sent after the budget was spent: %v", in)
		return &model.ReviewResult{}, nil
	}}
	count, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("findings = %d, want the truncated answer's one", count)
	}
	for _, s := range readAudit(t, cfg.AuditLog).FilesSkippedDetail {
		if s.Reason != SkipBudget {
			t.Errorf("%s skipped as %s, want budget", s.Path, s.Reason)
		}
	}
}

// A deleted file has no new path; it is tracked under its old path whether it
// was reviewed or skipped.
func TestChunkRun_DeletedFileIsTrackedUnderOneName(t *testing.T) {
	gone := diff.FileDiff{OldPath: "gone.go", IsDelete: true}
	newRun := func() *chunkRun {
		return &chunkRun{reviewed: map[string]bool{}, reviewedHunks: map[string]int{}, unreviewedPaths: map[string]bool{}}
	}

	run := newRun()
	run.accept([]diff.FileDiff{gone}, &model.ReviewResult{})
	if !run.reviewed["gone.go"] || run.reviewed[""] {
		t.Errorf("reviewed = %v, want gone.go only", run.reviewed)
	}

	run.skip([]diff.FileDiff{gone}, SkipModelError)
	if run.reviewed["gone.go"] {
		t.Error("a skipped deleted file is still counted as reviewed")
	}
	if len(run.unreviewed) != 1 || run.unreviewed[0].Path != "gone.go" {
		t.Errorf("unreviewed = %v", run.unreviewed)
	}
	if diffPath(gone) != "gone.go" {
		t.Errorf("diffPath = %q", diffPath(gone))
	}
}
