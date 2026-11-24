package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

// sysModel answers with fn and records the system and user prompt of every call.
type sysModel struct {
	fn func(system, user string) (*model.ReviewResult, error)

	mu    sync.Mutex
	calls []promptPair
}

type promptPair struct{ system, user string }

func (m *sysModel) Review(_ context.Context, system, user string) (*model.ReviewResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, promptPair{system, user})
	m.mu.Unlock()
	return m.fn(system, user)
}

func (m *sysModel) Close() {}

// crossCalls returns the calls of the cross-chunk step.
func (m *sysModel) crossCalls() []promptPair {
	var out []promptPair
	for _, c := range m.calls {
		if strings.Contains(c.system, "CROSS-CHUNK REVIEW") {
			out = append(out, c)
		}
	}
	return out
}

var crossFiles = []string{"a.go", "b.go", "c.go"}

// digestingModel gives each chunk a finding and a digest, and answers the
// cross-chunk step with cross.
func digestingModel(cross func(user string) (*model.ReviewResult, error)) *sysModel {
	return &sysModel{fn: func(system, user string) (*model.ReviewResult, error) {
		if strings.Contains(system, "CROSS-CHUNK REVIEW") {
			return cross(user)
		}
		f := chunkOf(user, crossFiles)
		return &model.ReviewResult{
			Findings: []model.Finding{findingFor(f)},
			Digest:   &model.ChunkDigest{SymbolsChanged: []string{"Handle in " + f}, ContractsTouched: []string{"wire format of " + f}},
		}, nil
	}}
}

func runCross(t *testing.T, enabled bool, m ModelReviewer, mutate func(*config.Config)) (int, error) {
	t.Helper()
	cfg := chunkCfg(t)
	cfg.CrossChunkReview = enabled
	if mutate != nil {
		mutate(cfg)
	}
	return NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(crossFiles...)}).Run(context.Background())
}

func TestRun_CrossChunkStepIsOffByDefault(t *testing.T) {
	m := digestingModel(func(string) (*model.ReviewResult, error) {
		t.Error("the cross-chunk step ran")
		return &model.ReviewResult{}, nil
	})
	count, err := runCross(t, false, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 3 || count != 3 {
		t.Errorf("calls = %d, findings = %d; want 3 chunk reviews and 3 findings", len(m.calls), count)
	}
	for _, c := range m.calls {
		if strings.Contains(c.system, "digest") {
			t.Error("a chunk was asked for a digest while the step is off")
		}
	}
}

func TestRun_CrossChunkStepSeesDigestsAndFindingsAndKeepsItsFindings(t *testing.T) {
	cross := func(user string) (*model.ReviewResult, error) {
		return &model.ReviewResult{Findings: []model.Finding{
			{File: "a.go", Line: 1, Severity: "HIGH", Category: "contract", Title: "caller in a.go broken by b.go", Body: "b.go changed the contract"},
			{File: "b.go", Line: 999, Severity: "HIGH", Category: "bug", Title: "off the diff", Body: "x"},   // Not on a changed line.
			{File: "c.go", Line: 1, Severity: "HIGH", Category: "bug", Title: "problem in c.go", Body: "x"}, // Repeats the chunk's finding.
		}}, nil
	}
	m := digestingModel(cross)
	count, err := runCross(t, true, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 4 {
		t.Fatalf("calls = %d, want 3 chunk reviews and the cross-chunk step", len(m.calls))
	}
	for _, c := range m.calls[:3] {
		if !strings.Contains(c.system, "digest") {
			t.Error("a chunk was not asked for a digest")
		}
	}
	got := m.crossCalls()
	if len(got) != 1 {
		t.Fatalf("cross-chunk calls = %d, want 1", len(got))
	}
	for _, want := range []string{"Handle in a.go", "Handle in c.go", "wire format of b.go", "problem in b.go", "a.go: 1"} {
		if !strings.Contains(got[0].user, want) {
			t.Errorf("cross-chunk prompt lacks %q:\n%s", want, got[0].user)
		}
	}
	// 3 chunk findings, +1 cross-chunk finding; the off-diff one is dropped and
	// the repeat is merged with the chunk's own.
	if count != 4 {
		t.Errorf("findings = %d, want 4", count)
	}
}

func TestRun_CrossChunkStepNeedsMoreThanOneChunk(t *testing.T) {
	m := digestingModel(func(string) (*model.ReviewResult, error) {
		t.Error("the cross-chunk step ran for a single chunk")
		return &model.ReviewResult{}, nil
	})
	cfg := chunkCfg(t)
	cfg.TokenLimit = 1_000_000
	cfg.CrossChunkReview = true
	if _, err := NewWithDiffSource(cfg, m, &mockVCS{}, &mockDiffSource{diffs: markedDiffs(crossFiles...)}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 {
		t.Errorf("calls = %d, want 1", len(m.calls))
	}
}

func TestRun_CrossChunkFailureNeverFailsTheRun(t *testing.T) {
	for name, cross := range map[string]func(string) (*model.ReviewResult, error){
		"error": func(string) (*model.ReviewResult, error) { return nil, errors.New("unavailable") },
		"fatal": func(string) (*model.ReviewResult, error) { return nil, model.MarkFatal(errors.New("denied")) },
		"truncated": func(string) (*model.ReviewResult, error) {
			return &model.ReviewResult{Truncated: true, Findings: []model.Finding{findingFor("a.go")}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := digestingModel(cross)
			count, err := runCross(t, true, m, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			want := 3
			if count != want {
				t.Errorf("findings = %d, want the chunks' %d untouched", count, want)
			}
		})
	}
}

func TestRun_CrossChunkStepIsBoundedByTheRemainingBudget(t *testing.T) {
	m := digestingModel(func(string) (*model.ReviewResult, error) { return &model.ReviewResult{}, nil })
	m2 := &sysModel{fn: func(system, user string) (*model.ReviewResult, error) {
		res, err := m.fn(system, user)
		if res != nil && !strings.Contains(system, "CROSS-CHUNK REVIEW") {
			res.Usage = &model.TokenUsage{TotalTokens: 1000}
		}
		return res, err
	}}
	// 3 chunks use 3000 of 3100; the step does not fit the 100 that are left.
	if _, err := runCross(t, true, m2, func(c *config.Config) { c.MaxTokens = 3100 }); err != nil {
		t.Fatal(err)
	}
	if n := len(m2.crossCalls()); n != 0 {
		t.Errorf("cross-chunk calls = %d, want none within a spent budget", n)
	}
}

func TestRun_CrossChunkDigestsAreBounded(t *testing.T) {
	long := strings.Repeat("y", 5000)
	var many []string
	for i := range 100 {
		many = append(many, fmt.Sprintf("sym%d", i))
	}
	m := &sysModel{fn: func(system, user string) (*model.ReviewResult, error) {
		return &model.ReviewResult{Digest: &model.ChunkDigest{SymbolsAdded: many, ContractsTouched: []string{long}}}, nil
	}}
	if _, err := runCross(t, true, m, nil); err != nil {
		t.Fatal(err)
	}
	got := m.crossCalls()
	if len(got) != 1 {
		t.Fatalf("cross-chunk calls = %d", len(got))
	}
	if strings.Contains(got[0].user, "sym40") || strings.Contains(got[0].user, long) {
		t.Error("digest entries are not bounded")
	}
	if !strings.Contains(got[0].user, "sym0") {
		t.Error("digest entries are missing")
	}
}

func TestChangedLineRanges(t *testing.T) {
	d := diff.FileDiff{NewPath: "x.go", Hunks: []diff.Hunk{{Lines: []diff.DiffLine{
		{Type: diff.LineContext, NewLineNo: 9},
		{Type: diff.LineAdded, NewLineNo: 10}, {Type: diff.LineAdded, NewLineNo: 11}, {Type: diff.LineRemoved},
		{Type: diff.LineAdded, NewLineNo: 12}, {Type: diff.LineContext, NewLineNo: 13},
		{Type: diff.LineAdded, NewLineNo: 20},
	}}}}
	if got := changedLineRanges(d); got != "10-12, 20" {
		t.Errorf("ranges = %q, want %q", got, "10-12, 20")
	}
}
