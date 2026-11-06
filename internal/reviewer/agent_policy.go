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
}

// toolCall is one tool invocation of a loop run.
type toolCall struct {
	tool string
	args map[string]any
	ok   bool
}

// toolCallLog records the tool calls of one run under ids the model can cite.
type toolCallLog struct {
	calls map[string]toolCall
}

func newToolCallLog() *toolCallLog { return &toolCallLog{calls: map[string]toolCall{}} }

// record stores a call and returns its id ("t1", "t2", ...).
func (l *toolCallLog) record(tool string, args map[string]any, ok bool) string {
	id := fmt.Sprintf("t%d", len(l.calls)+1)
	l.calls[id] = toolCall{tool: tool, args: args, ok: ok}
	return id
}

// anySucceeded reports whether any of the ids names a tool call that returned a result.
func (l *toolCallLog) anySucceeded(ids []string) bool {
	for _, id := range ids {
		if c, found := l.calls[strings.TrimSpace(id)]; found && c.ok {
			return true
		}
	}
	return false
}

// supports reports whether the evidence names a successful read_file call
// whose line range overlaps the cited lines of the same file.
func (l *toolCallLog) supports(ev evidenceRef) bool {
	c, found := l.calls[strings.TrimSpace(ev.ToolCallID)]
	if !found || !c.ok || c.tool != "read_file" {
		return false
	}
	readPath, _ := c.args["path"].(string)
	if readPath == "" || cleanRepoPath(readPath) != cleanRepoPath(ev.File) {
		return false
	}
	if ev.StartLine < 1 || ev.EndLine < ev.StartLine {
		return false
	}
	readStart, readEnd := 1, int(^uint(0)>>1)
	if n, ok := intArg(c.args["start_line"]); ok && n > 1 {
		readStart = n
	}
	if n, ok := intArg(c.args["end_line"]); ok && n > 0 {
		readEnd = n
	}
	return ev.StartLine <= readEnd && ev.EndLine >= readStart
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
	counts := make(map[locationKey]int, len(initial))
	for _, f := range initial {
		counts[locationKey{f.File, f.Line, f.OldLine}]++
	}
	for _, r := range returned {
		if r.Line > 0 {
			r.OldLine = 0
		}
		k := locationKey{r.File, r.Line, r.OldLine}
		if counts[k] > 0 {
			counts[k]--
			matched = append(matched, r)
		} else {
			unmatched = append(unmatched, r)
		}
	}
	return matched, unmatched
}

// applyClaimPolicy enforces unverifiable_claims on one returned finding from
// its structured fields. A finding that declares an external claim and cites
// no successful tool call is lowered to the lowest reported severity and gets
// the note; one whose severity a rule fixes keeps it and gets the note only.
// It reports whether the finding was lowered.
func (p agentPolicy) applyClaimPolicy(r refinedFinding, calls *toolCallLog) (model.Finding, bool) {
	f := r.Finding
	if !p.downgrade || r.ExternalClaim == nil || !*r.ExternalClaim || calls.anySucceeded(r.VerifiedBy) {
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
// added findings. Each needs a title, a body, a line, and evidence naming
// read_file calls of this run that cover its own file; a finding that
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
		if f.File == "" || f.Title == "" || f.Body == "" || (f.Line <= 0 && f.OldLine <= 0) {
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

// citedEvidence checks that every cited range is backed by a read_file call
// of this run and that one of them is in the finding's own file. It returns
// the citations as text.
func citedEvidence(f model.Finding, evidence []evidenceRef, calls *toolCallLog) (string, bool) {
	if len(evidence) == 0 {
		return "", false
	}
	ownFile := false
	cites := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		if !calls.supports(ev) {
			return "", false
		}
		if cleanRepoPath(ev.File) == cleanRepoPath(f.File) {
			ownFile = true
		}
		cites = append(cites, fmt.Sprintf("`%s:%d-%d`", cleanRepoPath(ev.File), ev.StartLine, ev.EndLine))
	}
	return strings.Join(cites, ", "), ownFile
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
			"each added finding must list in \"evidence\" the tool_call_id, file, start_line and end_line of the read_file call that shows the problem, "+
			"and one evidence item must be in the finding's own file. Do not add a finding you cannot cite, and do not repeat an initial finding.\n", p.maxAdded)
	} else {
		sb.WriteString("- You may ONLY drop or modify initial findings. You MUST NOT add new findings.\n")
	}
	sb.WriteString("- Only dismiss a finding if new context from tools PROVES it is a false positive.\n")
	sb.WriteString("- If you cannot verify a finding, keep it as-is.\n")
	sb.WriteString("- Use tools to read relevant source files and verify findings.\n")
	if p.downgrade {
		sb.WriteString("- Set \"external_claim\" to true on every finding that asserts a fact about something outside this repository: " +
			"a library, framework or provider API, option, version, runtime, URL format or language syntax. " +
			"For those, list in \"verified_by\" the ids of the tool calls whose results confirm the fact, and leave it empty when no tool result does. " +
			"A finding with an external claim that no tool result confirms is downgraded automatically.\n")
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
		sb.WriteString("- Finish with the findings action so every finding carries its external_claim and verified_by.\n\n")
	} else {
		sb.WriteString("- To finish without changes: {\"action\": \"done\"}\n\n")
	}
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
