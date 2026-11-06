package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpticDiff/code-reviewer/internal/config"
	"github.com/OpticDiff/code-reviewer/pkg/model"
)

// repoWithFile creates a repository root holding one file of numbered lines.
func repoWithFile(t *testing.T, name string, lines int) string {
	t.Helper()
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func toolCallReply(tool string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"action": "tool", "tool": tool, "args": args})
	return string(b)
}

func findingsReply(t *testing.T, findings ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"action": "findings", "findings": findings})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// wire builds the JSON form of a finding the way the loop returns it.
func wire(file string, line int, category, title string, extra map[string]any) map[string]any {
	m := map[string]any{
		"file": file, "line": line, "old_line": 0, "severity": "HIGH",
		"category": category, "title": title, "body": "body of " + title,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func evidence(id, file string, start, end int) []map[string]any {
	return []map[string]any{{"tool_call_id": id, "file": file, "start_line": start, "end_line": end}}
}

var exploreInitial = []model.Finding{{File: "a.go", Line: 5, Severity: "HIGH", Category: "bug", Title: "initial", Body: "b"}}

func runExplore(t *testing.T, cfg AgentConfig, replies ...string) *AgentResult {
	t.Helper()
	cfg.Mode = AgentModeExplore
	if cfg.RepoRoot == "" {
		cfg.RepoRoot = repoWithFile(t, "a.go", 60)
	}
	cfg.MaxIterations = 5
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: replies}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExplore_AddsAFindingCitingWhatItRead(t *testing.T) {
	res := runExplore(t, AgentConfig{},
		toolCallReply("read_file", map[string]any{"path": "a.go", "start_line": 10, "end_line": 30}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", nil),
			wire("a.go", 20, "security", "added", map[string]any{"evidence": evidence("t1", "a.go", 12, 18)}),
		),
	)
	if len(res.Findings) != 2 {
		t.Fatalf("findings = %+v, want the initial and one added finding", res.Findings)
	}
	initial, added := res.Findings[0], res.Findings[1]
	if initial.Origin != "" {
		t.Errorf("initial finding origin = %q, want none", initial.Origin)
	}
	if added.Origin != model.OriginAgentExplore || added.Title != "added" {
		t.Errorf("added finding = %+v, want origin %q", added, model.OriginAgentExplore)
	}
	if !strings.Contains(added.Body, "`a.go:12-18`") {
		t.Errorf("added body %q does not cite the lines read", added.Body)
	}
	if res.Added != 1 {
		t.Errorf("Added = %d, want 1", res.Added)
	}
	want := []string{VerdictVerified, VerdictAdded}
	if len(res.Verdicts) != 2 || res.Verdicts[0].Status != want[0] || res.Verdicts[1].Status != want[1] {
		t.Errorf("verdicts = %+v, want %v", res.Verdicts, want)
	}
}

func TestExplore_RejectsAddedFindingsWithoutCheckableEvidence(t *testing.T) {
	read := toolCallReply("read_file", map[string]any{"path": "a.go", "start_line": 10, "end_line": 30})
	cases := map[string]map[string]any{
		"no evidence":              nil,
		"unknown tool call":        {"evidence": evidence("t9", "a.go", 12, 18)},
		"lines outside the read":   {"evidence": evidence("t1", "a.go", 40, 50)},
		"other file than the read": {"evidence": evidence("t1", "b.go", 12, 18)},
		"empty line range":         {"evidence": evidence("t1", "a.go", 18, 12)},
		"evidence not in own file": {"evidence": evidence("t1", "a.go", 12, 18), "file": "c.go"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			res := runExplore(t, AgentConfig{}, read,
				findingsReply(t,
					wire("a.go", 5, "bug", "initial", nil),
					wire("a.go", 20, "security", "added", extra),
				),
			)
			if len(res.Findings) != 1 || res.Added != 0 {
				t.Errorf("findings = %+v, want only the initial finding", res.Findings)
			}
		})
	}
}

func TestExplore_EvidenceMustComeFromASuccessfulReadFile(t *testing.T) {
	res := runExplore(t, AgentConfig{},
		toolCallReply("list_directory", map[string]any{"path": "."}),
		toolCallReply("read_file", map[string]any{"path": "missing.go"}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", nil),
			wire("a.go", 20, "security", "listing", map[string]any{"evidence": evidence("t1", "a.go", 12, 18)}),
			wire("missing.go", 3, "security", "failed read", map[string]any{"evidence": evidence("t2", "missing.go", 1, 5)}),
		),
	)
	if len(res.Findings) != 1 {
		t.Errorf("findings = %+v, want evidence from list_directory and a failed read_file refused", res.Findings)
	}
}

func TestRefineMode_StillDropsAddedFindings(t *testing.T) {
	cfg := AgentConfig{RepoRoot: repoWithFile(t, "a.go", 60), MaxIterations: 5}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{
		toolCallReply("read_file", map[string]any{"path": "a.go"}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", nil),
			wire("a.go", 20, "security", "added", map[string]any{"evidence": evidence("t1", "a.go", 12, 18)}),
		),
	}}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Added != 0 {
		t.Errorf("findings = %+v, want the added finding dropped in refine mode", res.Findings)
	}
}

func TestExplore_CapsAddedFindingsPerRun(t *testing.T) {
	read := toolCallReply("read_file", map[string]any{"path": "a.go"})
	var added []map[string]any
	for i, line := range []int{20, 30, 40} {
		added = append(added, wire("a.go", line, "security", "added"+string(rune('A'+i)),
			map[string]any{"evidence": evidence("t1", "a.go", line, line+1)}))
	}
	res := runExplore(t, AgentConfig{MaxAddedFindings: 2}, read,
		findingsReply(t, append([]map[string]any{wire("a.go", 5, "bug", "initial", nil)}, added...)...))
	if res.Added != 2 || len(res.Findings) != 3 {
		t.Fatalf("findings = %+v, want the initial plus 2 added", res.Findings)
	}
	if res.Findings[1].Title != "addedA" || res.Findings[2].Title != "addedB" {
		t.Errorf("kept %q and %q, want the first two in order", res.Findings[1].Title, res.Findings[2].Title)
	}
}

func TestExplore_DefaultCapApplies(t *testing.T) {
	read := toolCallReply("read_file", map[string]any{"path": "a.go"})
	returned := []map[string]any{wire("a.go", 5, "bug", "initial", nil)}
	for i := 0; i < DefaultMaxAddedFindings+3; i++ {
		line := 10 + i*10
		returned = append(returned, wire("a.go", line, "security", "added",
			map[string]any{"evidence": evidence("t1", "a.go", line, line)}))
	}
	if res := runExplore(t, AgentConfig{}, read, findingsReply(t, returned...)); res.Added != DefaultMaxAddedFindings {
		t.Errorf("Added = %d, want the default cap %d", res.Added, DefaultMaxAddedFindings)
	}
}

func TestExplore_DedupsAgainstInitialAndEarlierAdditions(t *testing.T) {
	read := toolCallReply("read_file", map[string]any{"path": "a.go"})
	ev := func(line int) map[string]any { return map[string]any{"evidence": evidence("t1", "a.go", line, line)} }
	res := runExplore(t, AgentConfig{}, read,
		findingsReply(t,
			// The initial finding is dismissed, yet its location stays taken.
			wire("a.go", 7, "bug", "same place as the initial one", ev(7)),
			wire("a.go", 30, "security", "first", ev(30)),
			wire("a.go", 32, "security", "near the first, same category", ev(32)),
			wire("a.go", 31, "style", "near the first, other category", ev(31)),
		),
	)
	var titles []string
	for _, f := range res.Findings {
		titles = append(titles, f.Title)
	}
	want := []string{"first", "near the first, other category"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("findings = %v, want %v", titles, want)
	}
}

func TestRunScopedAgentLoop_AddedFindingsDedupAgainstUnexaminedOnes(t *testing.T) {
	findings := []model.Finding{
		{File: "a.go", Line: 5, Severity: "HIGH", Category: "bug", Title: "initial", Body: "b"},
		{File: "a.go", Line: 31, Severity: "LOW", Category: "security", Title: "low one", Body: "b"},
	}
	cfg := AgentConfig{Mode: AgentModeExplore, RepoRoot: repoWithFile(t, "a.go", 60), MaxIterations: 5}
	chatter := &scriptedChatter{replies: []string{
		toolCallReply("read_file", map[string]any{"path": "a.go"}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", nil),
			wire("a.go", 30, "security", "dup of the low one", map[string]any{"evidence": evidence("t1", "a.go", 30, 30)}),
		),
	}}
	got, _, err := runScopedAgentLoop(context.Background(), chatter, cfg, findings, AgentScopeHighSeverityOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || countAdded(got) != 0 {
		t.Errorf("findings = %+v, want the added duplicate of the unexamined finding dropped", got)
	}
}

func TestExplore_RunsPromptAndToolResultsCarryIDs(t *testing.T) {
	chatter := &recordingChatter{replies: []string{
		toolCallReply("list_directory", map[string]any{"path": "."}),
		`{"action":"done"}`,
	}}
	cfg := AgentConfig{Mode: AgentModeExplore, RepoRoot: t.TempDir()}
	if _, err := RunAgentLoop(context.Background(), chatter, cfg, exploreInitial); err != nil {
		t.Fatal(err)
	}
	last := chatter.histories[1]
	if got := last[len(last)-1].Parts[0].Text; !strings.Contains(got, `"id":"t1"`) {
		t.Errorf("tool result %q carries no id", got)
	}
}

func TestSettingsOff_ToolResultsCarryNoIDs(t *testing.T) {
	chatter := &recordingChatter{replies: []string{
		toolCallReply("list_directory", map[string]any{"path": "."}),
		`{"action":"done"}`,
	}}
	if _, err := RunAgentLoop(context.Background(), chatter, AgentConfig{RepoRoot: t.TempDir()}, exploreInitial); err != nil {
		t.Fatal(err)
	}
	last := chatter.histories[1]
	if got := last[len(last)-1].Parts[0].Text; strings.Contains(got, `"id"`) {
		t.Errorf("tool result %q carries an id with the settings off", got)
	}
}

// downgradeCase runs one finding through the loop with the downgrade policy.
func downgradeRun(t *testing.T, cfg AgentConfig, returned map[string]any, calls ...string) *AgentResult {
	t.Helper()
	cfg.UnverifiableClaims = UnverifiableClaimsDowngrade
	cfg.RepoRoot = repoWithFile(t, "a.go", 20)
	cfg.MaxIterations = 5
	replies := append(calls, findingsReply(t, returned))
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: replies}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDowngrade_UnconfirmedExternalClaimIsLoweredWithANote(t *testing.T) {
	for name, claim := range map[string]map[string]any{
		"no tool cited":     {"external_claim": true, "verified_by": []string{}},
		"unknown id":        {"external_claim": true, "verified_by": []string{"t7"}},
		"cited call failed": {"external_claim": true, "verified_by": []string{"t1"}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []string
			if name == "cited call failed" {
				calls = []string{toolCallReply("read_file", map[string]any{"path": "missing.go"})}
			}
			res := downgradeRun(t, AgentConfig{}, wire("a.go", 5, "bug", "initial", claim), calls...)
			f := res.Findings[0]
			if f.Severity != "LOW" || !strings.Contains(f.Body, "not verified against the pinned version") {
				t.Errorf("finding = %+v, want LOW with the note", f)
			}
			if res.Downgraded != 1 || res.Verdicts[0].Status != VerdictDowngraded {
				t.Errorf("Downgraded = %d, verdicts = %+v", res.Downgraded, res.Verdicts)
			}
		})
	}
}

func TestDowngrade_KeepsWhatAToolResultConfirmedOrThatClaimsNothingExternal(t *testing.T) {
	read := toolCallReply("read_file", map[string]any{"path": "a.go"})
	for name, claim := range map[string]map[string]any{
		"verified":     {"external_claim": true, "verified_by": []string{"t1"}},
		"not external": {"external_claim": false, "verified_by": []string{}},
		"flag missing": {},
	} {
		t.Run(name, func(t *testing.T) {
			res := downgradeRun(t, AgentConfig{}, wire("a.go", 5, "bug", "initial", claim), read)
			f := res.Findings[0]
			if f.Severity != "HIGH" || strings.Contains(f.Body, "not verified") {
				t.Errorf("finding = %+v, want it untouched", f)
			}
			if res.Downgraded != 0 || res.Verdicts[0].Status != VerdictVerified {
				t.Errorf("Downgraded = %d, verdicts = %+v", res.Downgraded, res.Verdicts)
			}
		})
	}
}

func TestDowngrade_AnyConfirmingCallSuffices(t *testing.T) {
	claim := map[string]any{"external_claim": true, "verified_by": []string{"t9", "t1"}}
	res := downgradeRun(t, AgentConfig{}, wire("a.go", 5, "bug", "initial", claim),
		toolCallReply("read_file", map[string]any{"path": "a.go"}))
	if res.Findings[0].Severity != "HIGH" {
		t.Errorf("severity = %q, want HIGH kept", res.Findings[0].Severity)
	}
}

func TestKeepPolicy_IgnoresExternalClaimFields(t *testing.T) {
	claim := map[string]any{"external_claim": true, "verified_by": []string{}}
	cfg := AgentConfig{RepoRoot: repoWithFile(t, "a.go", 20)}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{
		findingsReply(t, wire("a.go", 5, "bug", "initial", claim)),
	}}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	if f := res.Findings[0]; f.Severity != "HIGH" || strings.Contains(f.Body, "not verified") {
		t.Errorf("finding = %+v, want it untouched under unverifiable_claims: keep", f)
	}
}

func TestDowngrade_SeverityLockedFindingKeepsSeverityAndGetsTheNote(t *testing.T) {
	cfg := AgentConfig{SeverityLocked: func(f model.Finding) bool { return f.RuleName == "no-eval" }}
	claim := map[string]any{"external_claim": true, "verified_by": []string{}, "rule_name": "no-eval"}
	res := downgradeRun(t, cfg, wire("a.go", 5, "bug", "initial", claim))
	f := res.Findings[0]
	if f.Severity != "HIGH" || !strings.Contains(f.Body, "not verified against the pinned version") {
		t.Errorf("finding = %+v, want severity kept and the note added", f)
	}
	if res.Downgraded != 0 {
		t.Errorf("Downgraded = %d, want 0 for a locked severity", res.Downgraded)
	}
}

func TestDowngrade_AppliesToAddedFindingsToo(t *testing.T) {
	cfg := AgentConfig{Mode: AgentModeExplore, UnverifiableClaims: UnverifiableClaimsDowngrade,
		RepoRoot: repoWithFile(t, "a.go", 60), MaxIterations: 5}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{
		toolCallReply("read_file", map[string]any{"path": "a.go"}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", map[string]any{"external_claim": false, "verified_by": []string{}}),
			wire("a.go", 20, "security", "added", map[string]any{
				"external_claim": true, "verified_by": []string{}, "evidence": evidence("t1", "a.go", 20, 21)}),
		),
	}}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	added := res.Findings[1]
	if added.Severity != "LOW" || !strings.Contains(added.Body, "not verified against the pinned version") {
		t.Errorf("added finding = %+v, want it lowered with the note", added)
	}
	if res.Downgraded != 1 {
		t.Errorf("Downgraded = %d, want 1", res.Downgraded)
	}
}

func TestDowngrade_DoneActionIsNotOfferedInTheSchema(t *testing.T) {
	reg := NewToolRegistry(t.TempDir())
	for _, b := range agentResponseSchema(reg, true, agentPolicy{downgrade: true}).AnyOf {
		if b.Properties["action"].Enum[0] == "done" {
			t.Error("the done action must not be offered when every answer needs per-finding claim fields")
		}
	}
}

func TestSchemaAndPrompt_SettingsOnMatchGolden(t *testing.T) {
	reg := NewToolRegistry(t.TempDir())
	policies := map[string]agentPolicy{
		"explore":           {explore: true, maxAdded: 3},
		"downgrade":         {downgrade: true},
		"explore_downgrade": {explore: true, maxAdded: 3, downgrade: true},
	}
	for name, p := range policies {
		schema, err := json.MarshalIndent(agentResponseSchema(reg, true, p), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if string(schema)+"\n" != readGolden(t, "agent_schema_"+name+".golden.json") {
			t.Errorf("%s response schema differs from its golden:\n%s", name, schema)
		}
		prompt := buildRefinementSystemPrompt(`[{"file":"a.go"}]`, reg.Prompt(), p)
		if prompt != readGolden(t, "refinement_prompt_"+name+".golden.txt") {
			t.Errorf("%s prompt differs from its golden:\n%s", name, prompt)
		}
	}
}

func TestSettingsOff_PromptAndStartInstructionAreUnchanged(t *testing.T) {
	var p agentPolicy
	explicit := AgentConfig{Mode: AgentModeRefine, UnverifiableClaims: UnverifiableClaimsKeep}.policy()
	reg := NewToolRegistry(t.TempDir())
	if got := buildRefinementSystemPrompt(`[{"file":"a.go"}]`, reg.Prompt(), explicit); got != readGolden(t, "refinement_prompt.golden.txt") {
		t.Errorf("explicit refine/keep prompt differs from the default golden:\n%s", got)
	}
	if got := (AgentConfig{}).policy(); got.explore || got.downgrade || got.tracksToolCalls() {
		t.Fatalf("zero config policy = %+v, want everything off", got)
	}
	want := "Review the initial findings above. Use the available tools to verify or dismiss each finding. " +
		"Only dismiss a finding if you can prove it is a false positive with concrete evidence from the code."
	if p.startInstruction() != want {
		t.Errorf("start instruction = %q", p.startInstruction())
	}
}

func TestReviewer_AgentConfigCarriesExploreSettings(t *testing.T) {
	r := &Reviewer{cfg: &config.Config{
		AgentMode: "explore", AgentMaxAdded: 4, UnverifiableClaims: "downgrade",
		Rules: []config.Rule{{Name: "No-Eval"}},
	}}
	got := r.agentConfig(1)
	if got.Mode != AgentModeExplore || got.MaxAddedFindings != 4 || got.UnverifiableClaims != UnverifiableClaimsDowngrade {
		t.Errorf("agent config = %+v", got)
	}
	if !got.SeverityLocked(model.Finding{RuleName: "no-eval"}) || got.SeverityLocked(model.Finding{RuleName: "other"}) || got.SeverityLocked(model.Finding{}) {
		t.Error("SeverityLocked must follow the configured rule names, ignoring case")
	}
}

func TestValidateAddedFindings_OnlyChecksAddedOnes(t *testing.T) {
	findings := []model.Finding{
		{File: "x.go", Line: 999, Title: "from the review pass"},
		{File: "x.go", Line: 999, Title: "added off the diff", Origin: model.OriginAgentExplore},
	}
	got := validateAddedFindings(findings, nil)
	if len(got) != 1 || got[0].Title != "from the review pass" {
		t.Errorf("findings = %+v, want the off-diff added finding dropped and the other left", got)
	}
}

func TestNewAgentAudit_CarriesAddedAndDowngraded(t *testing.T) {
	a := newAgentAudit(&AgentResult{StopReason: "done", Added: 2, Downgraded: 1}, nil, 3, 5)
	if a.Added != 2 || a.Downgraded != 1 {
		t.Errorf("audit = %+v", a)
	}
}
