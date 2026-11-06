package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			wire("a.go", 20, "security", "added", map[string]any{"evidence": evidence("t1", "a.go", 12, 24)}),
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
	if !strings.Contains(added.Body, "`a.go:12-24`") {
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

// evidenceTool is an external-evidence tool (like a dependency or MCP tool)
// that answers with a fixed text.
func evidenceTool(name, answer string, inconclusive func(string) bool) Tool {
	return Tool{
		Name: name, Description: "test lookup", InputSchema: ObjectSchema(map[string]any{}),
		UntrustedOutput: true, ExternalEvidence: true, Inconclusive: inconclusive,
		Execute: func(context.Context, map[string]any) (string, error) { return answer, nil },
	}
}

func notInstalled(out string) bool { return strings.HasPrefix(out, "not installed") }

// citeCall runs the scripted tool calls, then returns a finding with an
// external claim that cites the id of the last call.
func citeCall(t *testing.T, tools []Tool, calls ...string) *AgentResult {
	t.Helper()
	claim := map[string]any{"external_claim": true, "verified_by": []string{fmt.Sprintf("t%d", len(calls))}}
	cfg := AgentConfig{ExtraTools: tools, UnverifiableClaims: UnverifiableClaimsDowngrade,
		RepoRoot: repoWithFile(t, "a.go", 20), MaxIterations: 10}
	replies := append(calls, findingsReply(t, wire("a.go", 5, "bug", "initial", claim)))
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: replies}, cfg, exploreInitial)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Only a conclusive answer of a dependency or MCP tool confirms an external
// claim: not a repository tool, not a failed call, and not an answer that says
// the tool found nothing.
func TestDowngrade_OnlyConclusiveExternalEvidenceConfirmsAClaim(t *testing.T) {
	tools := []Tool{
		evidenceTool("lookup", "version 2.0 exists", notInstalled),
		evidenceTool("lookup_missing", "not installed: left-pad", notInstalled),
		evidenceTool("lookup_blank", "  \n", nil),
		{Name: "lookup_error", Description: "fails", InputSchema: ObjectSchema(map[string]any{}), ExternalEvidence: true,
			Execute: func(context.Context, map[string]any) (string, error) { return "", errors.New("boom") }},
	}
	cases := []struct {
		name      string
		call      string
		confirmed bool
	}{
		{"conclusive answer", toolCallReply("lookup", map[string]any{}), true},
		{"answer that found nothing", toolCallReply("lookup_missing", map[string]any{}), false},
		{"blank answer", toolCallReply("lookup_blank", map[string]any{}), false},
		{"failed call", toolCallReply("lookup_error", map[string]any{}), false},
		{"read_file of the repository", toolCallReply("read_file", map[string]any{"path": "a.go"}), false},
		{"list_directory", toolCallReply("list_directory", map[string]any{"path": "."}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := citeCall(t, tools, c.call)
			lowered := res.Findings[0].Severity == "LOW"
			if lowered == c.confirmed {
				t.Errorf("severity = %q, confirmed = %v", res.Findings[0].Severity, c.confirmed)
			}
		})
	}
}

func TestDowngrade_DependencyToolsConfirmOnlyWhenTheyFoundSomething(t *testing.T) {
	repo := repoWithFile(t, "a.go", 20)
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"dependencies":{"react":"^19.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		call      string
		confirmed bool
	}{
		{"pin found", toolCallReply("pinned_version", map[string]any{"package": "react"}), true},
		{"no pin found", toolCallReply("pinned_version", map[string]any{"package": "left-pad"}), false},
		{"not installed", toolCallReply("read_dependency_source", map[string]any{"package": "react"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claim := map[string]any{"external_claim": true, "verified_by": []string{"t1"}}
			cfg := AgentConfig{DependencyTools: []string{"pinned_version", "read_dependency_source"},
				UnverifiableClaims: UnverifiableClaimsDowngrade, RepoRoot: repo, MaxIterations: 5}
			res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{c.call, findingsReply(t, wire("a.go", 5, "bug", "initial", claim))}}, cfg, exploreInitial)
			if err != nil {
				t.Fatal(err)
			}
			if lowered := res.Findings[0].Severity == "LOW"; lowered == c.confirmed {
				t.Errorf("severity = %q, confirmed = %v", res.Findings[0].Severity, c.confirmed)
			}
		})
	}
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
			if f.Severity != "LOW" || !strings.Contains(f.Body, "not verified with a tool") {
				t.Errorf("finding = %+v, want LOW with the note", f)
			}
			if res.Downgraded != 1 || res.Verdicts[0].Status != VerdictDowngraded {
				t.Errorf("Downgraded = %d, verdicts = %+v", res.Downgraded, res.Verdicts)
			}
		})
	}
}

func TestDowngrade_KeepsWhatAToolResultConfirmedOrThatClaimsNothingExternal(t *testing.T) {
	read := toolCallReply("lookup", map[string]any{})
	for name, claim := range map[string]map[string]any{
		"verified":     {"external_claim": true, "verified_by": []string{"t1"}},
		"not external": {"external_claim": false, "verified_by": []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			res := downgradeRun(t, AgentConfig{ExtraTools: []Tool{evidenceTool("lookup", "documented", nil)}}, wire("a.go", 5, "bug", "initial", claim), read)
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

// A finding that never says whether it asserts an outside fact cannot be told
// apart from one that does, so it is treated as an unconfirmed claim.
func TestDowngrade_FindingWithoutAClaimIsTreatedAsUnconfirmed(t *testing.T) {
	res := downgradeRun(t, AgentConfig{}, wire("a.go", 5, "bug", "initial", nil))
	if f := res.Findings[0]; f.Severity != "LOW" || !strings.Contains(f.Body, "not verified") {
		t.Errorf("finding = %+v, want LOW with the note", f)
	}
}

func TestDowngrade_AnyConfirmingCallSuffices(t *testing.T) {
	claim := map[string]any{"external_claim": true, "verified_by": []string{"t9", "t1"}}
	res := downgradeRun(t, AgentConfig{ExtraTools: []Tool{evidenceTool("lookup", "documented", nil)}}, wire("a.go", 5, "bug", "initial", claim),
		toolCallReply("lookup", map[string]any{}))
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
	if f.Severity != "HIGH" || !strings.Contains(f.Body, "not verified with a tool") {
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
	if added.Severity != "LOW" || !strings.Contains(added.Body, "not verified with a tool") {
		t.Errorf("added finding = %+v, want it lowered with the note", added)
	}
	if res.Downgraded != 1 {
		t.Errorf("Downgraded = %d, want 1", res.Downgraded)
	}
}

func TestDowngrade_SchemaOffersDoneAndClaimsExceptOnTheFinalTurn(t *testing.T) {
	reg := NewToolRegistry(t.TempDir())
	actions := func(withTools bool) map[string]bool {
		got := map[string]bool{}
		for _, b := range agentResponseSchema(reg, withTools, agentPolicy{downgrade: true}).AnyOf {
			got[b.Properties["action"].Enum[0]] = true
		}
		return got
	}
	if a := actions(true); !a["done"] || !a["claims"] || !a["findings"] || !a["tool"] {
		t.Errorf("tool turns offer %v, want tool, findings, claims and done", a)
	}
	if a := actions(false); !a["done"] || !a["findings"] || a["claims"] || a["tool"] {
		t.Errorf("final turn offers %v, want findings and done only", a)
	}
	for _, b := range agentResponseSchema(reg, true, agentPolicy{}).AnyOf {
		if b.Properties["action"].Enum[0] == "claims" {
			t.Error("claims action offered without unverifiable_claims: downgrade")
		}
	}
	// The claim fields on a finding are optional: claims can report them instead.
	for _, b := range agentResponseSchema(reg, true, agentPolicy{downgrade: true}).AnyOf {
		if b.Properties["action"].Enum[0] != "findings" {
			continue
		}
		item := b.Properties["findings"].Items
		if _, ok := item.Properties["external_claim"]; !ok {
			t.Error("findings can no longer carry external_claim")
		}
		for _, r := range item.Required {
			if r == "external_claim" || r == "verified_by" {
				t.Errorf("%s is required on every finding; the claims action exists so they need not be echoed", r)
			}
		}
	}
}

func claimsReply(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"action": "claims", "claims": entries})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func claim(index int, external bool, verifiedBy ...string) map[string]any {
	if verifiedBy == nil {
		verifiedBy = []string{}
	}
	return map[string]any{"index": index, "external_claim": external, "verified_by": verifiedBy}
}

var twoInitial = []model.Finding{
	{File: "a.go", Line: 5, Severity: "HIGH", Category: "bug", Title: "first", Body: "b"},
	{File: "a.go", Line: 9, Severity: "HIGH", Category: "bug", Title: "second", Body: "b"},
}

func claimsRun(t *testing.T, replies ...string) *AgentResult {
	t.Helper()
	cfg := AgentConfig{
		UnverifiableClaims: UnverifiableClaimsDowngrade, RepoRoot: repoWithFile(t, "a.go", 20), MaxIterations: 8,
		ExtraTools: []Tool{evidenceTool("lookup", "documented", nil)},
	}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: replies}, cfg, twoInitial)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func severities(res *AgentResult) string {
	var out []string
	for _, f := range res.Findings {
		out = append(out, f.Severity)
	}
	return strings.Join(out, ",")
}

func TestClaims_ThenDoneJudgesEachFindingWithoutEchoingIt(t *testing.T) {
	res := claimsRun(t,
		toolCallReply("lookup", map[string]any{}),
		claimsReply(t, claim(0, true, "t1"), claim(1, true)),
		`{"action":"done"}`,
	)
	if got := severities(res); got != "HIGH,LOW" {
		t.Errorf("severities = %s, want the confirmed finding kept and the unconfirmed one lowered", got)
	}
	if res.StopReason != "done" || res.Downgraded != 1 || res.Verdicts[1].Status != VerdictDowngraded || res.Verdicts[0].Status != VerdictVerified {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Findings[1].Body, "not verified") || strings.Contains(res.Findings[0].Body, "not verified") {
		t.Errorf("notes on the wrong findings: %+v", res.Findings)
	}
}

func TestClaims_NotExternalFindingsAreLeftAlone(t *testing.T) {
	res := claimsRun(t, claimsReply(t, claim(0, false), claim(1, false)), `{"action":"done"}`)
	if got := severities(res); got != "HIGH,HIGH" || res.Downgraded != 0 {
		t.Errorf("severities = %s, downgraded = %d, want both kept", got, res.Downgraded)
	}
}

// done with no claims leaves nothing to tell the findings apart by, so the
// downgrade setting treats them all as unconfirmed.
func TestClaims_DoneWithoutClaimsLowersEveryUnclaimedFinding(t *testing.T) {
	res := claimsRun(t, `{"action":"done"}`)
	if got := severities(res); got != "LOW,LOW" || res.Downgraded != 2 {
		t.Errorf("severities = %s, downgraded = %d, want both lowered", got, res.Downgraded)
	}
	partial := claimsRun(t, claimsReply(t, claim(1, false)), `{"action":"done"}`)
	if got := severities(partial); got != "LOW,HIGH" {
		t.Errorf("only the reported finding should be spared: %s", got)
	}
}

func TestClaims_LaterEntriesReplaceEarlierOnesAndBadIndexesAreIgnored(t *testing.T) {
	res := claimsRun(t,
		claimsReply(t, claim(0, false), claim(7, true), claim(-1, true)),
		claimsReply(t, claim(0, true)),
		claimsReply(t, claim(1, false)),
		`{"action":"done"}`,
	)
	if got := severities(res); got != "LOW,HIGH" {
		t.Errorf("severities = %s, want the second report on finding 0 to win", got)
	}
}

func TestClaims_FindingsActionUsesRecordedClaimsAndInlineFieldsWin(t *testing.T) {
	res := claimsRun(t,
		claimsReply(t, claim(0, false), claim(1, false)),
		findingsReply(t,
			wire("a.go", 5, "bug", "first", nil), // judged by the recorded claim: not external
			wire("a.go", 9, "bug", "second", map[string]any{"external_claim": true, "verified_by": []string{}}), // inline fields win
		),
	)
	if got := severities(res); got != "HIGH,LOW" {
		t.Errorf("severities = %s", got)
	}
}

func TestClaims_FindingsAtOneLocationAreJudgedByTheirOwnPosition(t *testing.T) {
	twin := []model.Finding{
		{File: "a.go", Line: 5, Severity: "HIGH", Category: "bug", Title: "one", Body: "b"},
		{File: "a.go", Line: 5, Severity: "HIGH", Category: "bug", Title: "two", Body: "b"},
	}
	cfg := AgentConfig{UnverifiableClaims: UnverifiableClaimsDowngrade, RepoRoot: repoWithFile(t, "a.go", 20), MaxIterations: 4}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{
		claimsReply(t, claim(0, false), claim(1, true)),
		findingsReply(t, wire("a.go", 5, "bug", "one", nil), wire("a.go", 5, "bug", "two", nil)),
	}}, cfg, twin)
	if err != nil {
		t.Fatal(err)
	}
	if got := severities(res); got != "HIGH,LOW" {
		t.Errorf("severities = %s, want the claim of each position applied to the finding at that position", got)
	}
}

func TestClaims_ActionWithoutTheDowngradePolicyStopsTheLoop(t *testing.T) {
	cfg := AgentConfig{RepoRoot: repoWithFile(t, "a.go", 20), MaxIterations: 4}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{claimsReply(t, claim(0, true))}}, cfg, twoInitial)
	if err != nil || res.StopReason != "error" || severities(res) != "HIGH,HIGH" {
		t.Errorf("res = %+v, err = %v", res, err)
	}
}

func TestClaims_NothingChangesOnABudgetOrIterationStop(t *testing.T) {
	cfg := AgentConfig{UnverifiableClaims: UnverifiableClaimsDowngrade, RepoRoot: repoWithFile(t, "a.go", 20), MaxIterations: 1}
	res, err := RunAgentLoop(context.Background(), &scriptedChatter{replies: []string{claimsReply(t, claim(0, true))}}, cfg, twoInitial)
	if err != nil || severities(res) != "HIGH,HIGH" || res.Downgraded != 0 {
		t.Errorf("res = %+v, err = %v; want findings untouched when the loop never finished", res, err)
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

// A read_file call may ask for more lines than the file holds. What it
// returned is the span a citation must stay within.
func TestExplore_CitationMustStayWithinTheLinesReadFileReturned(t *testing.T) {
	cases := []struct {
		name       string
		read       map[string]any
		file       string
		line       int
		start, end int
		accepted   bool
	}{
		{"whole read inside the file", map[string]any{"path": "a.go", "start_line": 1, "end_line": 99999}, "a.go", 20, 1, 60, true},
		{"[1,99999] cites lines the file does not have", map[string]any{"path": "a.go", "start_line": 1, "end_line": 99999}, "a.go", 20, 1, 99999, false},
		{"end past the last line", map[string]any{"path": "a.go", "start_line": 1, "end_line": 99999}, "a.go", 20, 10, 61, false},
		{"requested range past the file end is clipped", map[string]any{"path": "a.go", "start_line": 50, "end_line": 500}, "a.go", 55, 50, 60, true},
		{"cite wider than the requested range", map[string]any{"path": "a.go", "start_line": 10, "end_line": 30}, "a.go", 20, 5, 30, false},
		{"cite exactly the requested range", map[string]any{"path": "a.go", "start_line": 10, "end_line": 30}, "a.go", 20, 10, 30, true},
		{"read without a range holds the whole file", map[string]any{"path": "a.go"}, "a.go", 60, 1, 60, true},
		{"a finding's own line must be inside the cited range", map[string]any{"path": "a.go", "start_line": 1, "end_line": 60}, "a.go", 40, 10, 20, false},
		{"own line on the first cited line", map[string]any{"path": "a.go", "start_line": 1, "end_line": 60}, "a.go", 10, 10, 20, true},
		{"own line on the last cited line", map[string]any{"path": "a.go", "start_line": 1, "end_line": 60}, "a.go", 20, 10, 20, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runExplore(t, AgentConfig{}, toolCallReply("read_file", c.read),
				findingsReply(t,
					wire("a.go", 5, "bug", "initial", nil),
					wire(c.file, c.line, "security", "added", map[string]any{"evidence": evidence("t1", c.file, c.start, c.end)}),
				),
			)
			if got := res.Added == 1; got != c.accepted {
				t.Errorf("added = %d, want accepted=%v (findings %+v)", res.Added, c.accepted, res.Findings)
			}
		})
	}
}

func TestExplore_OwnLineMustBeCoveredByEvidenceInTheSameFile(t *testing.T) {
	root := repoWithFile(t, "a.go", 60)
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte(strings.Repeat("x\n", 60)), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runExplore(t, AgentConfig{RepoRoot: root},
		toolCallReply("read_file", map[string]any{"path": "b.go"}),
		toolCallReply("read_file", map[string]any{"path": "a.go", "start_line": 40, "end_line": 50}),
		findingsReply(t,
			wire("a.go", 5, "bug", "initial", nil),
			// Line 20 of a.go is covered by b.go's span, not by a read of a.go.
			wire("a.go", 20, "security", "added", map[string]any{"evidence": append(evidence("t1", "b.go", 10, 30), evidence("t2", "a.go", 40, 50)...)}),
		),
	)
	if res.Added != 0 {
		t.Errorf("added = %d, want 0: a finding's own line must be inside a cited range of its own file", res.Added)
	}
}

func TestReadFileSpan(t *testing.T) {
	cases := []struct {
		name   string
		args   map[string]any
		output string
		want   lineSpan
	}{
		{"whole file", map[string]any{}, "a\nb\nc\n", lineSpan{1, 3}},
		{"no trailing newline", map[string]any{}, "a\nb\nc", lineSpan{1, 3}},
		{"from a start line", map[string]any{"start_line": float64(10)}, "a\nb", lineSpan{10, 11}},
		{"start below 1", map[string]any{"start_line": float64(-4)}, "a", lineSpan{1, 1}},
		{"truncated read", map[string]any{}, "a\nb\n[... truncated at 32 KiB ...]", lineSpan{1, 2}},
		{"empty output", map[string]any{"start_line": 7}, "", lineSpan{7, 6}},
	}
	for _, c := range cases {
		if got := readFileSpan(c.args, c.output); got != c.want {
			t.Errorf("%s: readFileSpan = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestExplore_AddedFindingsAreCappedInSeverity(t *testing.T) {
	cases := []struct {
		name     string
		cap      string
		severity string
		locked   func(model.Finding) bool
		want     string
	}{
		{"default cap lowers HIGH", "", "HIGH", nil, "MEDIUM"},
		{"default cap lowers CRITICAL", "", "CRITICAL", nil, "MEDIUM"},
		{"default cap keeps MEDIUM", "", "MEDIUM", nil, "MEDIUM"},
		{"default cap keeps LOW", "", "LOW", nil, "LOW"},
		{"configured cap high", "HIGH", "CRITICAL", nil, "HIGH"},
		{"configured cap low", "low", "HIGH", nil, "LOW"},
		{"unknown cap falls back to the default", "urgent", "HIGH", nil, "MEDIUM"},
		{"a rule that fixes the severity keeps it", "", "HIGH", func(model.Finding) bool { return true }, "HIGH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runExplore(t, AgentConfig{AddedMaxSeverity: c.cap, SeverityLocked: c.locked},
				toolCallReply("read_file", map[string]any{"path": "a.go"}),
				findingsReply(t,
					wire("a.go", 5, "bug", "initial", nil),
					wire("a.go", 20, "security", "added", map[string]any{"severity": c.severity, "evidence": evidence("t1", "a.go", 15, 25)}),
				),
			)
			if len(res.Findings) != 2 {
				t.Fatalf("findings = %+v", res.Findings)
			}
			if got := res.Findings[1].Severity; got != c.want {
				t.Errorf("added severity = %s, want %s", got, c.want)
			}
			if got := res.Findings[0].Severity; got != "HIGH" {
				t.Errorf("the initial finding's severity changed to %s", got)
			}
		})
	}
}
