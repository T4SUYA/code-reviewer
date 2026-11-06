package reviewer

import (
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/pkg/model"
)

// Values accepted for Config.AgentMode.
const (
	// AgentModeRefine lets the agent loop only drop or modify the findings it is given.
	AgentModeRefine = "refine"
	// AgentModeExplore lets the loop also add findings it establishes with tool evidence.
	AgentModeExplore = "explore"
)

// Values accepted for Config.UnverifiableClaims.
const (
	// UnverifiableClaimsKeep leaves a finding whose external claim is unconfirmed as it is.
	UnverifiableClaimsKeep = "keep"
	// UnverifiableClaimsDowngrade lowers a finding whose external claim is unconfirmed.
	UnverifiableClaimsDowngrade = "downgrade"
)

// DefaultMaxAddedFindings caps the findings one explore run may add when
// AgentConfig.MaxAddedFindings is unset.
const DefaultMaxAddedFindings = 5

// Outcomes of the loop beyond verified and dismissed.
const (
	// VerdictAdded marks a finding the loop added in explore mode.
	VerdictAdded = "added"
	// VerdictDowngraded marks a finding lowered because its claim about
	// something outside the repository was not confirmed by a tool result.
	VerdictDowngraded = "downgraded"
)

const (
	// downgradedSeverity is the lowest severity a finding can have and still
	// be reported.
	downgradedSeverity = "LOW"
	// unverifiedClaimNote is appended to the body of a finding whose external
	// claim no tool result confirmed.
	unverifiedClaimNote = "Note: not verified against the pinned version."
)

// agentPolicy is the opt-in behavior of one agent loop run. The zero value is
// the refine-only loop with unconfirmed claims kept.
type agentPolicy struct {
	explore   bool
	maxAdded  int
	downgrade bool
	// locked reports whether a finding's severity is fixed by a configured
	// rule, so a downgrade must not change it.
	locked func(model.Finding) bool
}

func (c AgentConfig) policy() agentPolicy {
	p := agentPolicy{
		explore:   c.Mode == AgentModeExplore,
		maxAdded:  c.MaxAddedFindings,
		downgrade: c.UnverifiableClaims == UnverifiableClaimsDowngrade,
		locked:    c.SeverityLocked,
	}
	if p.maxAdded <= 0 {
		p.maxAdded = DefaultMaxAddedFindings
	}
	return p
}

// tracksToolCalls reports whether tool results carry an id the findings can cite.
func (p agentPolicy) tracksToolCalls() bool { return p.explore || p.downgrade }

// evidenceRef is a range of a file the loop read, cited by an added finding.
type evidenceRef struct {
	ToolCallID string `json:"tool_call_id"`
	File       string `json:"file"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
}

// refinedFinding is a finding as the loop returns it: the finding itself plus
// the structured fields the opt-in settings ask for.
type refinedFinding struct {
	model.Finding
	// ExternalClaim is true when the finding asserts a fact about something
	// outside the repository. A nil value means the model did not say.
	ExternalClaim *bool `json:"external_claim,omitempty"`
	// VerifiedBy lists the ids of the tool calls whose results confirm the claim.
	VerifiedBy []string `json:"verified_by,omitempty"`
	// Evidence lists what an added finding is based on.
	Evidence []evidenceRef `json:"evidence,omitempty"`
	// initialIndex is the position of the initial finding this one refines,
	// or -1 when it matches none.
	initialIndex int
}

// claimEntry is what the claims action says about one initial finding.
type claimEntry struct {
	Index         int      `json:"index"`
	ExternalClaim *bool    `json:"external_claim"`
	VerifiedBy    []string `json:"verified_by"`
}

// judged returns the finding with the claim it is judged by: the fields the
// finding itself carries when it says whether its claim is external, else the
// entry the claims action recorded for its position, else none.
func (r refinedFinding) judged(recorded map[int]claimEntry) refinedFinding {
	if r.ExternalClaim != nil {
		return r
	}
	if e, ok := recorded[r.initialIndex]; ok && r.initialIndex >= 0 {
		r.ExternalClaim, r.VerifiedBy = e.ExternalClaim, e.VerifiedBy
	}
	return r
}

// lineSpan is a range of file lines, both ends included. An empty span has end < start.
type lineSpan struct{ start, end int }

func (s lineSpan) contains(start, end int) bool { return start >= s.start && end <= s.end }

// toolCall is one tool invocation of a loop run.
type toolCall struct {
	tool string
	args map[string]any
	ok   bool
	// confirms is true when the call ran an external-evidence tool and its
	// answer was conclusive.
	confirms bool
	// span is the lines of the file a successful read_file call returned.
	span lineSpan
}

// toolCallLog records the tool calls of one run under ids the model can cite.
type toolCallLog struct {
	calls map[string]toolCall
}

func newToolCallLog() *toolCallLog { return &toolCallLog{calls: map[string]toolCall{}} }

// record stores a call and its result and returns its id ("t1", "t2", ...).
func (l *toolCallLog) record(tool string, args map[string]any, res ToolResult) string {
	id := fmt.Sprintf("t%d", len(l.calls)+1)
	c := toolCall{tool: tool, args: args, ok: res.Error == "", confirms: res.Confirms}
	if c.ok && tool == toolReadFile {
		c.span = readFileSpan(args, res.Output)
	}
	l.calls[id] = c
	return id
}

const (
	toolReadFile = "read_file"
	// readFileTruncationMarker ends the output of a read_file call cut at its size cap.
	readFileTruncationMarker = "\n[... truncated at 32 KiB ...]"
)

// readFileSpan returns the file lines a read_file result holds. The call may
// ask for more lines than the file has (or than the size cap lets through),
// so the span comes from the returned text, not from the arguments.
func readFileSpan(args map[string]any, output string) lineSpan {
	start := 1
	if n, ok := intArg(args["start_line"]); ok && n > 1 {
		start = n
	}
	body := strings.TrimSuffix(output, readFileTruncationMarker)
	if body == "" {
		return lineSpan{start: start, end: start - 1}
	}
	lines := strings.Count(body, "\n") + 1
	if strings.HasSuffix(body, "\n") {
		lines-- // The final newline ends the last line; it does not open another.
	}
	return lineSpan{start: start, end: start + lines - 1}
}

// anyConfirms reports whether any of the ids names a call to a dependency or
// MCP tool that gave a conclusive answer. A call that only found nothing
// ("not installed", "no pin found", "package exists: no", "no documentation
// found"), a failed call, and a call to a repository tool do not count: none
// of them confirms a fact about something outside the repository.
func (l *toolCallLog) anyConfirms(ids []string) bool {
	for _, id := range ids {
		if c, found := l.calls[strings.TrimSpace(id)]; found && c.ok && c.confirms {
			return true
		}
	}
	return false
}

// supports reports whether the evidence names a successful read_file call of
// the same file whose returned lines include every cited line.
func (l *toolCallLog) supports(ev evidenceRef) bool {
	c, found := l.calls[strings.TrimSpace(ev.ToolCallID)]
	if !found || !c.ok || c.tool != toolReadFile {
		return false
	}
	readPath, _ := c.args["path"].(string)
	if readPath == "" || cleanRepoPath(readPath) != cleanRepoPath(ev.File) {
		return false
	}
	if ev.StartLine < 1 || ev.EndLine < ev.StartLine {
		return false
	}
	return c.span.contains(ev.StartLine, ev.EndLine)
}

func intArg(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func cleanRepoPath(p string) string {
	return strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, "\\", "/")), "./")
}

// locationKey identifies where a finding sits, on the new or the old side.
type locationKey struct {
	File    string
	Line    int
	OldLine int
}

// partitionByInitial splits the loop's returned findings into those that
// match an initial finding by location (count-based, so duplicates at one
// location stay balanced) and the rest. A returned finding on a new-side line
// ignores its old_line.
func partitionByInitial(initial []model.Finding, returned []refinedFinding) (matched, unmatched []refinedFinding) {
	positions := make(map[locationKey][]int, len(initial))
	for i, f := range initial {
		k := locationKey{f.File, f.Line, f.OldLine}
		positions[k] = append(positions[k], i)
	}
	for _, r := range returned {
		if r.Line > 0 {
			r.OldLine = 0
		}
		k := locationKey{r.File, r.Line, r.OldLine}
		if idx := positions[k]; len(idx) > 0 {
			r.initialIndex = idx[0]
			positions[k] = idx[1:]
			matched = append(matched, r)
		} else {
			r.initialIndex = -1
			unmatched = append(unmatched, r)
		}
	}
	return matched, unmatched
}

// applyClaimPolicy enforces unverifiable_claims on one finding from its
// structured claim (see judged). A finding that does not say it is free of
// external claims, and cites no conclusive dependency or MCP call, is treated
// as an unconfirmed external claim: it is lowered to the lowest reported
// severity and gets the note, and one whose severity a rule fixes keeps it and
// gets the note only. Treating an unclassified finding this way is
// deliberately conservative: when the loop finishes without saying which
// findings assert outside facts, none of them can be told apart from one that
// does. It reports whether the finding was lowered.
func (p agentPolicy) applyClaimPolicy(r refinedFinding, calls *toolCallLog) (model.Finding, bool) {
	f := r.Finding
	if !p.downgrade || (r.ExternalClaim != nil && !*r.ExternalClaim) || calls.anyConfirms(r.VerifiedBy) {
		return f, false
	}
	if !strings.Contains(f.Body, unverifiedClaimNote) {
		f.Body = strings.TrimRight(f.Body, "\n") + "\n\n" + unverifiedClaimNote
	}
	if p.locked != nil && p.locked(f) {
		return f, false
	}
	f.Severity = downgradedSeverity
	return f, true
}

// acceptAdded turns the returned findings that match no initial finding into
// added findings. Each needs a title, a body, a line in the working tree, and
// evidence naming read_file calls of this run whose returned lines include
// the cited range, one of which contains the finding's own line; a finding that
// duplicates an initial, retained or earlier added finding (same file and
// category within three lines) is dropped, and at most p.maxAdded are kept.
func (p agentPolicy) acceptAdded(initial, retained []model.Finding, candidates []refinedFinding, calls *toolCallLog) (added []model.Finding, downgraded int) {
	known := append(append([]model.Finding{}, initial...), retained...)
	for _, c := range candidates {
		f := c.Finding
		if len(added) >= p.maxAdded {
			slog.Debug("agent loop: added-finding cap reached", "cap", p.maxAdded)
			break
		}
		if f.File == "" || f.Title == "" || f.Body == "" || f.Line <= 0 {
			slog.Debug("agent loop: dropping added finding without location or text", "title", f.Title)
			continue
		}
		cited, ok := citedEvidence(f, c.Evidence, calls)
		if !ok {
			slog.Debug("agent loop: dropping added finding without tool evidence", "file", f.File, "line", f.Line)
			continue
		}
		if matchesAny(f, known) {
			slog.Debug("agent loop: dropping added finding that duplicates another", "file", f.File, "line", f.Line)
			continue
		}
		c.Finding = f
		out, lowered := p.applyClaimPolicy(c, calls)
		if lowered {
			downgraded++
		}
		out.Origin = model.OriginAgentExplore
		out.Body = strings.TrimRight(out.Body, "\n") + "\n\nEvidence read by the agent loop: " + cited
		added = append(added, out)
		known = append(known, out)
	}
	return added, downgraded
}

// citedEvidence checks that every cited range lies within the lines a
// read_file call of this run returned, and that a cited range in the
// finding's own file contains the finding's own line. It returns the
// citations as text.
func citedEvidence(f model.Finding, evidence []evidenceRef, calls *toolCallLog) (string, bool) {
	if len(evidence) == 0 || f.Line <= 0 {
		return "", false
	}
	coversOwnLine := false
	cites := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		if !calls.supports(ev) {
			return "", false
		}
		if cleanRepoPath(ev.File) == cleanRepoPath(f.File) && ev.StartLine <= f.Line && f.Line <= ev.EndLine {
			coversOwnLine = true
		}
		cites = append(cites, fmt.Sprintf("`%s:%d-%d`", cleanRepoPath(ev.File), ev.StartLine, ev.EndLine))
	}
	return strings.Join(cites, ", "), coversOwnLine
}

func matchesAny(f model.Finding, others []model.Finding) bool {
	for _, o := range others {
		if model.FindingsMatch(f, o) {
			return true
		}
	}
	return false
}

// refinementRules is the rules section of the refinement system prompt for
// the given policy. The refine-only policy yields the original three lines
// around the tool guidance.
func (p agentPolicy) refinementRules() string {
	var sb strings.Builder
	sb.WriteString("## Rules\n")
	if p.explore {
		fmt.Fprintf(&sb, "- You may drop or modify initial findings. You may also ADD at most %d new findings, but only ones you establish with tool evidence: "+
			"each added finding must list in \"evidence\" the tool_call_id, file, start_line and end_line of the read_file call that shows the problem. "+
			"Cite only lines that call returned, and one evidence range must contain the finding's own line. "+
			"Do not add a finding you cannot cite, and do not repeat an initial finding.\n", p.maxAdded)
	} else {
		sb.WriteString("- You may ONLY drop or modify initial findings. You MUST NOT add new findings.\n")
	}
	sb.WriteString("- Only dismiss a finding if new context from tools PROVES it is a false positive.\n")
	sb.WriteString("- If you cannot verify a finding, keep it as-is.\n")
	sb.WriteString("- Use tools to read relevant source files and verify findings.\n")
	if p.downgrade {
		sb.WriteString("- Before you finish, report with the claims action which findings assert a fact about something outside this repository: " +
			"a library, framework or provider API, option, version, runtime, URL format or language syntax. " +
			"Findings are numbered from 0 in the order of <initial_findings>. " +
			"For each, give \"index\", \"external_claim\" (true or false) and \"verified_by\": the ids of the dependency or MCP tool calls whose conclusive results confirm the fact, empty when none does. " +
			"A call that found nothing (\"not installed\", \"no pin found\", \"package exists: no\", \"no documentation found\") confirms nothing, " +
			"and reading repository files does not confirm a fact about a dependency. " +
			"A finding you do not report on, or whose external claim no tool result confirms, is downgraded automatically.\n")
	}
	if p.tracksToolCalls() {
		sb.WriteString("- Every tool result carries an \"id\" you can cite.\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// responseFormat is the response format section of the refinement prompt.
func (p agentPolicy) responseFormat() string {
	var sb strings.Builder
	sb.WriteString("## Response Format\n")
	sb.WriteString("Respond with exactly ONE JSON object per turn:\n")
	sb.WriteString("- To call a tool: {\"action\": \"tool\", \"tool\": \"<name>\", \"args\": {<args>}}\n")
	if p.explore {
		sb.WriteString("- To return refined and added findings: {\"action\": \"findings\", \"findings\": [<findings>]}\n")
	} else {
		sb.WriteString("- To return refined findings: {\"action\": \"findings\", \"findings\": [<findings>]}\n")
	}
	if p.downgrade {
		sb.WriteString("- To report which findings assert outside facts, once you have your evidence: " +
			"{\"action\": \"claims\", \"claims\": [{\"index\": 0, \"external_claim\": true, \"verified_by\": [\"t1\"]}]}\n")
	}
	sb.WriteString("- To finish without changes: {\"action\": \"done\"}\n\n")
	return sb.String()
}

// startInstruction is the first user message of the loop.
func (p agentPolicy) startInstruction() string {
	msg := "Review the initial findings above. Use the available tools to verify or dismiss each finding. " +
		"Only dismiss a finding if you can prove it is a false positive with concrete evidence from the code."
	if p.explore {
		msg += " You may also add findings you can back with evidence from files you read."
	}
	return msg
}

// dropAddedMatching removes the agent-added findings that duplicate one in others.
func dropAddedMatching(findings, others []model.Finding) []model.Finding {
	kept := findings[:0:0]
	for _, f := range findings {
		if f.Origin == model.OriginAgentExplore && matchesAny(f, others) {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// validateAddedFindings checks the findings the loop added against the diff
// the way the review pass's findings are checked, and leaves the rest as they are.
func validateAddedFindings(findings []model.Finding, diffs []diff.FileDiff) []model.Finding {
	var added []model.Finding
	for _, f := range findings {
		if f.Origin == model.OriginAgentExplore {
			added = append(added, f)
		}
	}
	if len(added) == 0 {
		return findings
	}
	validated := ValidateFindings(added, diffs)
	out := make([]model.Finding, 0, len(findings))
	for _, f := range findings {
		if f.Origin != model.OriginAgentExplore {
			out = append(out, f)
		}
	}
	for i := range validated {
		validated[i].Origin = model.OriginAgentExplore
	}
	return append(out, validated...)
}

// countAdded counts the findings the loop added.
func countAdded(findings []model.Finding) int {
	n := 0
	for _, f := range findings {
		if f.Origin == model.OriginAgentExplore {
			n++
		}
	}
	return n
}
