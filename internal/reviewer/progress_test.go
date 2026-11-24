package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/OpticDiff/code-reviewer/internal/model"
	pkgmodel "github.com/OpticDiff/code-reviewer/pkg/model"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// captureLogs returns the log records written at info level and above while
// the test runs, each as its key/value map.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.b.String()), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

func recordsWith(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func TestRun_LogsWhenEachChunkFinishes(t *testing.T) {
	logs := captureLogs(t)
	files := []string{"a.go", "b.go", "c.go"}
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		switch chunkOf(prompt, files) {
		case "b.go":
			return nil, errors.New("unreadable answer")
		}
		return &model.ReviewResult{Findings: []model.Finding{findingFor(chunkOf(prompt, files))}}, nil
	}}
	cfg := chunkCfg(t)
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished := recordsWith(logs(), "chunk finished")
	if len(finished) != 3 {
		t.Fatalf("got %d chunk-finished lines, want 3: %v", len(finished), finished)
	}
	want := []struct {
		outcome  string
		findings float64
	}{{"reviewed", 1}, {"skipped", 0}, {"reviewed", 1}}
	for i, w := range want {
		r := finished[i]
		if r["chunk"] != float64(i+1) || r["of"] != float64(3) || r["files"] != float64(1) {
			t.Errorf("line %d = %v, want chunk %d of 3 with 1 file", i, r, i+1)
		}
		if r["outcome"] != w.outcome || r["findings"] != w.findings {
			t.Errorf("line %d: outcome %v findings %v, want %s and %v", i, r["outcome"], r["findings"], w.outcome, w.findings)
		}
		if _, ok := r["duration_ms"]; !ok {
			t.Errorf("line %d has no duration_ms", i)
		}
	}
	// The failed chunk's retry is logged too.
	if len(recordsWith(logs(), "chunk review failed")) == 0 {
		t.Error("the failed chunk was not logged")
	}
}

func TestRun_LogsAChunkThatWasSplit(t *testing.T) {
	logs := captureLogs(t)
	files := []string{"a.go", "b.go"}
	m := &funcModel{fn: func(prompt string) (*model.ReviewResult, error) {
		if len(markersIn(prompt, files)) > 1 {
			return nil, model.MarkBadAnswer(errors.New("too much"))
		}
		return &model.ReviewResult{}, nil
	}}
	if _, err := NewWithDiffSource(oneChunkCfg(t), m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(files...)}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	finished := recordsWith(logs(), "chunk finished")
	if len(finished) != 1 || finished[0]["outcome"] != "split" || finished[0]["files"] != float64(2) {
		t.Fatalf("chunk-finished lines = %v, want one split chunk of 2 files", finished)
	}
	if len(recordsWith(logs(), "reviewing the chunk again in parts")) != 1 {
		t.Error("the split was not logged")
	}
}

func TestRun_LogsTheCrossChunkStep(t *testing.T) {
	logs := captureLogs(t)
	m := digestingModel(func(string) (*model.ReviewResult, error) { return &model.ReviewResult{}, nil })
	if _, err := runCross(t, true, m, nil); err != nil {
		t.Fatal(err)
	}
	recs := logs()
	if len(recordsWith(recs, "cross-chunk step started")) != 1 {
		t.Error("the start of the cross-chunk step was not logged")
	}
	done := recordsWith(recs, "cross-chunk step finished")
	if len(done) != 1 {
		t.Fatalf("the end of the cross-chunk step was not logged: %v", recs)
	}
	if _, ok := done[0]["duration_ms"]; !ok {
		t.Error("the cross-chunk step end has no duration_ms")
	}
}

func TestRunAgentLoop_LogsEachIteration(t *testing.T) {
	logs := captureLogs(t)
	initial := []pkgmodel.Finding{{File: "a.go", Line: 1, Severity: "HIGH", Title: "kept"}}
	refined, _ := json.Marshal(initial)
	chatter := &scriptedChatter{replies: []string{`{"action":"findings","findings":` + string(refined) + `}`}}
	if _, err := RunAgentLoop(context.Background(), chatter, AgentConfig{RepoRoot: t.TempDir()}, initial); err != nil {
		t.Fatal(err)
	}
	its := recordsWith(logs(), "agent loop: iteration finished")
	if len(its) != 1 || its[0]["iteration"] != float64(1) || its[0]["action"] != "findings" {
		t.Fatalf("iteration lines = %v", its)
	}
	if _, ok := its[0]["duration_ms"]; !ok {
		t.Error("the iteration has no duration_ms")
	}
}
