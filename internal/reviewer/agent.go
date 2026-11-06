// Package reviewer provides the agent refinement loop for multi-turn code review.
package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/deptools"
	"github.com/OpticDiff/code-reviewer/internal/mcp"
	providermodel "github.com/OpticDiff/code-reviewer/internal/model"
	"github.com/OpticDiff/code-reviewer/internal/retry"
	"github.com/OpticDiff/code-reviewer/pkg/model"
	"google.golang.org/genai"
)

// Chatter supports multi-turn conversation for the agent loop.
// This is separate from ModelReviewer (which is stateless sys+user)
// because the agent loop needs to maintain conversation history
// including tool call results across turns.
type Chatter interface {
	Chat(ctx context.Context, history []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

// GenaiChatter wraps a genai.Client to satisfy the Chatter interface.
type GenaiChatter struct {
	Client    *genai.Client
	ModelName string
}

// Chat sends a multi-turn conversation to the model.
func (g *GenaiChatter) Chat(ctx context.Context, history []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	var result *genai.GenerateContentResponse
	var genErr error

	retryOpts := retry.DefaultOptions()
	retryOpts.RetryIf = func(err error) bool {
		errStr := strings.ToLower(err.Error())
		return strings.Contains(errStr, "429") ||
			strings.Contains(errStr, "503") ||
			strings.Contains(errStr, "502") ||
			strings.Contains(errStr, "504") ||
			strings.Contains(errStr, "rate") ||
			strings.Contains(errStr, "unavailable") ||
			strings.Contains(errStr, "overloaded")
	}

	if err := retry.Do(ctx, "agent chat", func() error {
		result, genErr = g.Client.Models.GenerateContent(ctx, g.ModelName, history, config)
		return genErr
	}, retryOpts); err != nil {
		return nil, fmt.Errorf("agent chat: %w", err)
	}
	return result, nil
}

// AgentConfig configures the agent refinement loop.
type AgentConfig struct {
	MaxIterations   int                // Max refinement turns (default 3).
	RemainingBudget int64              // Token budget remaining from initial review.
	Timeout         time.Duration      // Wall-clock timeout (default 3m).
	RepoRoot        string             // Repository root for tool access.
	OmitTemperature bool               // Send no temperature, leaving the model's own default (default false: 0.0).
	ThinkingLevel   string             // Thinking level sent on every turn: minimal, low, medium or high ("" = send none).
	ExtraTools      []Tool             // Tools registered after the built-in repository tools.
	MCPServers      []mcp.ServerConfig // MCP servers whose allowlisted tools register as "<server>.<tool>".
	// DependencyTools names the dependency tools to register (pinned_version,
	// registry_lookup, read_dependency_source, docs_lookup). Empty registers none.
	DependencyTools       []string
	DependencyToolOptions deptools.Options // Options of the dependency tools; RepoRoot is filled from this config.

	Mode               string                   // AgentModeRefine (default) or AgentModeExplore: the loop may also add findings backed by tool evidence.
	MaxAddedFindings   int                      // Findings an explore run may add (default DefaultMaxAddedFindings).
	UnverifiableClaims string                   // UnverifiableClaimsKeep (default) or UnverifiableClaimsDowngrade.
	SeverityLocked     func(model.Finding) bool // Reports findings whose severity a rule fixes; a downgrade leaves their severity alone.
}

// AgentResult holds the output of the agent refinement loop.
type AgentResult struct {
	Findings   []model.Finding  `json:"findings"`
	Usage      model.TokenUsage `json:"usage"`
	Iterations int              `json:"iterations"`
	ToolCalls  map[string]int   `json:"tool_calls"`           // tool name -> call count
	StopReason string           `json:"stop_reason"`          // "done", "budget", "iterations", "timeout", "error", "stable"
	Added      int              `json:"added,omitempty"`      // Findings the loop added (explore mode).
	Downgraded int              `json:"downgraded,omitempty"` // Findings lowered for an unconfirmed external claim.
	// Verdicts records, for every finding the loop was given, whether it was
	// verified, dismissed, or left unverified because the loop did not finish.
	Verdicts []FindingVerdict `json:"verdicts,omitempty"`
}

// AgentAudit summarises one agent loop run for the audit log, so a loop that
// stopped early or never got an answer is visible without reading stderr.
type AgentAudit struct {
	Iterations     int            `json:"iterations"`
	StopReason     string         `json:"stop_reason"`
	ToolCalls      map[string]int `json:"tool_calls,omitempty"`
	FindingsBefore int            `json:"findings_before"`
	FindingsAfter  int            `json:"findings_after"`
	Added          int            `json:"added,omitempty"`
	Downgraded     int            `json:"downgraded,omitempty"`
	Error          string         `json:"error,omitempty"`
}

// newAgentAudit builds the audit summary, or nil when the loop never ran.
func newAgentAudit(res *AgentResult, err error, before, after int) *AgentAudit {
	if res == nil && err == nil {
		return nil
	}
	a := &AgentAudit{FindingsBefore: before, FindingsAfter: after, StopReason: "error"}
	if res != nil {
		a.Iterations, a.StopReason, a.ToolCalls = res.Iterations, res.StopReason, res.ToolCalls
		a.Added, a.Downgraded = res.Added, res.Downgraded
	}
	if err != nil {
		a.Error = err.Error()
	}
	return a
}

// Agent loop verdict statuses.
const (
	// VerdictVerified marks a finding that survived a completed agent loop.
	VerdictVerified = "verified"
	// VerdictDismissed marks a finding the agent loop refuted and dropped.
	VerdictDismissed = "dismissed"
	// VerdictUnverified marks a finding the loop could not rule on, for
	// example because it hit its budget, timeout or an error first.
	VerdictUnverified = "unverified"
)

// Values accepted for Config.AgentScope.
const (
	// AgentScopeAll sends every finding through the agent loop.
	AgentScopeAll = "all"
	// AgentScopeHighSeverityOnly sends only HIGH and CRITICAL findings through the agent loop.
	AgentScopeHighSeverityOnly = "high_severity_only"
)

// FindingVerdict is the agent loop's outcome for a single finding, written to the audit log.
type FindingVerdict struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	OldLine  int    `json:"old_line,omitempty"` // Set instead of Line for a finding on a removed line.
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Status   string `json:"status"` // VerdictVerified, VerdictDismissed or VerdictUnverified.
}

// splitAgentCandidates partitions findings into those the agent loop should
// examine under the given scope and those it should leave untouched.
func splitAgentCandidates(findings []model.Finding, scope string) (candidates, rest []model.Finding) {
	if scope != AgentScopeHighSeverityOnly {
		return findings, nil
	}
	for _, f := range findings {
		if severityRank(f.Severity) >= severityRank("HIGH") {
			candidates = append(candidates, f)
		} else {
			rest = append(rest, f)
		}
	}
	return candidates, rest
}

// buildVerdicts labels each initial finding by whether it survives in the
// final findings. When the loop did not complete, nothing is ruled on.
func buildVerdicts(initial, final []model.Finding, completed bool) []FindingVerdict {
	type key struct {
		File    string
		Line    int
		OldLine int
	}
	retained := make(map[key]int, len(final))
	for _, f := range final {
		retained[key{f.File, f.Line, f.OldLine}]++
	}
	verdicts := make([]FindingVerdict, 0, len(initial))
	for _, f := range initial {
		k := key{f.File, f.Line, f.OldLine}
		status := VerdictUnverified
		switch {
		case !completed:
		case retained[k] > 0:
			retained[k]--
			status = VerdictVerified
		default:
			status = VerdictDismissed
		}
		verdicts = append(verdicts, FindingVerdict{File: f.File, Line: f.Line, OldLine: f.OldLine, Severity: f.Severity, Title: f.Title, Status: status})
	}
	return verdicts
}

// agentAction is the parsed model response format.
type agentAction struct {
	Action   string          `json:"action"`             // "tool", "findings", "done"
	Tool     string          `json:"tool,omitempty"`     // tool name (when action=tool)
	Args     map[string]any  `json:"args,omitempty"`     // tool arguments
	Findings json.RawMessage `json:"findings,omitempty"` // refined findings (when action=findings)
}

// RunAgentLoop executes the agent refinement loop. It takes initial findings,
// uses tool calls to gather additional context, and returns refined findings.
// If the loop fails, callers should fall back to the initial findings.
func RunAgentLoop(ctx context.Context, chatter Chatter, cfg AgentConfig,
	initialFindings []model.Finding,
) (*AgentResult, error) {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Minute
	}
	if cfg.RemainingBudget <= 0 {
		cfg.RemainingBudget = 50000
	}

	// Enforce wall-clock timeout (from reliability review).
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	tools := NewToolRegistry(cfg.RepoRoot)
	for _, t := range cfg.ExtraTools {
		if err := tools.Register(t); err != nil {
			return nil, fmt.Errorf("registering agent tool: %w", err)
		}
	}
	if err := registerDependencyTools(tools, cfg); err != nil {
		return nil, err
	}
	if len(cfg.MCPServers) > 0 {
		defer registerMCPTools(ctx, tools, cfg.MCPServers)()
	}
	result := &AgentResult{
		Findings:   initialFindings,
		ToolCalls:  make(map[string]int),
		StopReason: "iterations",
	}

	// Build initial findings JSON for system prompt.
	findingsJSON, err := json.Marshal(initialFindings)
	if err != nil {
		return nil, fmt.Errorf("marshaling initial findings: %w", err)
	}

	// System instruction: pin initial findings (from ML review — don't rely on event history).
	policy := cfg.policy()
	calls := newToolCallLog()
	systemPrompt := buildRefinementSystemPrompt(string(findingsJSON), tools.Prompt(), policy)
	genConfig := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
		ResponseMIMEType:  "application/json",
		ResponseSchema:    agentResponseSchema(tools, true, policy),
	}
	if !cfg.OmitTemperature {
		genConfig.Temperature = genai.Ptr(float32(0.0)) // Deterministic for refinement (ML review).
	}
	genConfig.ThinkingConfig = providermodel.ThinkingConfigFor(cfg.ThinkingLevel)
	finalConfig := *genConfig
	finalConfig.ResponseSchema = agentResponseSchema(tools, false, policy)

	// Build conversation history.
	history := []*genai.Content{
		genai.NewContentFromText(policy.startInstruction(), genai.RoleUser),
	}

	var totalUsage model.TokenUsage
	consecutiveToolErrors := 0
	var retained, downgraded, addedFindings []model.Finding
	answered := false

	for i := 0; i < cfg.MaxIterations; i++ {
		// Check budget before calling model.
		if totalUsage.TotalTokens >= cfg.RemainingBudget {
			result.StopReason = "budget"
			slog.Info("agent loop: budget exhausted", "used", totalUsage.TotalTokens, "budget", cfg.RemainingBudget)
			break
		}

		// Check context (timeout).
		if ctx.Err() != nil {
			result.StopReason = "timeout"
			slog.Info("agent loop: timeout reached")
			break
		}

		turnConfig := genConfig
		if i == cfg.MaxIterations-1 {
			turnConfig = &finalConfig
		}
		resp, err := chatter.Chat(ctx, history, turnConfig)
		if err != nil {
			result.StopReason = "error"
			result.Usage = totalUsage // Preserve usage accumulated so far.
			result.Verdicts = buildVerdicts(initialFindings, result.Findings, false)
			return result, fmt.Errorf("agent iteration %d: %w", i+1, err)
		}
		result.Iterations = i + 1

		// Track token usage.
		if resp.UsageMetadata != nil {
			totalUsage.InputTokens += int64(resp.UsageMetadata.PromptTokenCount)
			totalUsage.OutputTokens += int64(resp.UsageMetadata.CandidatesTokenCount)
			totalUsage.TotalTokens += int64(resp.UsageMetadata.TotalTokenCount)
		}

		// Extract model response text.
		modelText := extractResponseText(resp)
		if modelText == "" {
			// Surfaced as an error: breaking out silently would let a model that
			// can never answer look like a loop that verified every finding.
			result.StopReason = "error"
			result.Usage = totalUsage
			result.Verdicts = buildVerdicts(initialFindings, result.Findings, false)
			return result, fmt.Errorf("agent iteration %d: empty model response (%s)", i+1, describeEmptyResponse(resp))
		}

		// Append assistant turn to history.
		history = append(history, genai.NewContentFromText(modelText, genai.RoleModel))

		// Parse the model's action.
		var action agentAction
		if err := json.Unmarshal([]byte(modelText), &action); err != nil {
			slog.Warn("agent loop: unparseable response, stopping", "iteration", i+1, "error", err)
			result.StopReason = "error"
			break
		}

		switch action.Action {
		case "tool":
			if action.Tool == "" {
				slog.Warn("agent loop: tool action with empty tool name", "iteration", i+1)
				consecutiveToolErrors++
				history = append(history, genai.NewContentFromText(
					"Tool call error: the tool name is empty. Choose a valid tool or return findings.",
					genai.RoleUser,
				))
				if consecutiveToolErrors >= 2 {
					slog.Warn("agent loop: too many consecutive tool errors, stopping")
					result.StopReason = "error"
					goto done
				}
				continue
			}
			result.ToolCalls[action.Tool]++

			// Execute tool.
			toolResult := tools.Execute(ctx, action.Tool, action.Args)
			if policy.tracksToolCalls() {
				toolResult.ID = calls.record(action.Tool, action.Args, toolResult)
			}
			if toolResult.Error != "" {
				consecutiveToolErrors++
				if consecutiveToolErrors >= 2 {
					slog.Warn("agent loop: too many consecutive tool errors, stopping",
						"tool", action.Tool, "error", toolResult.Error)
					result.StopReason = "error"
					goto done
				}
			} else {
				consecutiveToolErrors = 0
			}

			// Append tool result to history as user message.
			resultText := fmt.Sprintf("<tool_result>%s</tool_result>", toolResultJSON(toolResult))
			if i+1 == cfg.MaxIterations-1 {
				resultText += "\nThis is the last turn: no more tool calls. Return the refined findings or done."
			}
			history = append(history, genai.NewContentFromText(resultText, genai.RoleUser))

		case "findings":
			// Parse refined findings.
			var returned []refinedFinding
			if err := json.Unmarshal(action.Findings, &returned); err != nil {
				slog.Warn("agent loop: invalid findings JSON, keeping initial", "error", err)
				result.StopReason = "error"
				goto done
			}
			// Findings at an initial location are refinements; the rest are
			// dropped unless explore mode accepts them with evidence.
			matched, unmatched := partitionByInitial(initialFindings, returned)
			answered = true
			for _, m := range matched {
				f, lowered := policy.applyClaimPolicy(m, calls)
				f.Origin = ""
				if lowered {
					downgraded = append(downgraded, f)
				}
				retained = append(retained, f)
			}
			final := append([]model.Finding{}, retained...)
			if policy.explore {
				added, loweredAdded := policy.acceptAdded(initialFindings, retained, unmatched, calls)
				result.Added, result.Downgraded = len(added), loweredAdded
				addedFindings = added
				final = append(final, added...)
			} else if len(unmatched) > 0 {
				slog.Debug("agent loop: dropping non-initial findings", "count", len(unmatched))
			}
			result.Findings = final
			result.StopReason = "done"
			goto done

		case "done":
			result.StopReason = "done"
			goto done

		default:
			slog.Warn("agent loop: unknown action", "action", action.Action, "iteration", i+1)
			result.StopReason = "error"
			goto done
		}
	}

done:
	result.Usage = totalUsage
	if answered {
		result.Verdicts = buildVerdicts(initialFindings, retained, true)
		result.Downgraded += len(downgraded)
		markDowngraded(result.Verdicts, downgraded)
		for _, f := range addedFindings {
			result.Verdicts = append(result.Verdicts, FindingVerdict{File: f.File, Line: f.Line, OldLine: f.OldLine, Severity: f.Severity, Title: f.Title, Status: VerdictAdded})
		}
	} else {
		result.Verdicts = buildVerdicts(initialFindings, result.Findings, result.StopReason == "done")
	}
	return result, nil
}

// runScopedAgentLoop runs the agent loop over the findings selected by scope
// and returns them merged with the findings the scope excluded. When nothing
// is selected the model is not called and the result is nil. On error the
// original findings are returned unchanged.
func runScopedAgentLoop(ctx context.Context, chatter Chatter, cfg AgentConfig,
	findings []model.Finding, scope string,
) ([]model.Finding, *AgentResult, error) {
	candidates, rest := splitAgentCandidates(findings, scope)
	if len(candidates) == 0 {
		return findings, nil, nil
	}
	result, err := RunAgentLoop(ctx, chatter, cfg, candidates)
	if err != nil {
		return findings, result, err
	}
	merged := append([]model.Finding{}, result.Findings...)
	if len(rest) > 0 {
		merged = dropAddedMatching(merged, rest)
	}
	return append(merged, rest...), result, nil
}

// buildRefinementSystemPrompt constructs the system prompt with pinned findings
// and tool schemas. Initial findings are in the system instruction so they
// survive memory compression (from ML Systems review).
func buildRefinementSystemPrompt(findingsJSON, toolsPrompt string, policy agentPolicy) string {
	var sb strings.Builder
	sb.WriteString("You are a code review refinement agent. Your job is to verify or dismiss initial findings.\n\n")
	sb.WriteString(policy.refinementRules())
	sb.WriteString(policy.responseFormat())
	sb.WriteString("## ")
	sb.WriteString(toolsPrompt)
	sb.WriteString("\n\n")
	sb.WriteString("## Initial Findings\n")
	sb.WriteString("<initial_findings>\n")
	sb.WriteString(findingsJSON)
	sb.WriteString("\n</initial_findings>\n")
	return sb.String()
}

// validateRefinedFindings ensures refined findings are a subset of initial findings.
// Uses count-based matching to handle duplicate findings at the same file+line.
// Any finding in refined that doesn't match an initial finding is dropped.
func validateRefinedFindings(initial, refined []model.Finding) []model.Finding {
	returned := make([]refinedFinding, len(refined))
	for i, f := range refined {
		returned[i] = refinedFinding{Finding: f}
	}
	matched, dropped := partitionByInitial(initial, returned)
	for _, d := range dropped {
		slog.Debug("agent loop: dropping non-initial finding", "file", d.File, "line", d.Line)
	}
	validated := make([]model.Finding, 0, len(matched))
	for _, m := range matched {
		validated = append(validated, m.Finding)
	}
	return validated
}

// markDowngraded relabels the verdict of each downgraded finding.
func markDowngraded(verdicts []FindingVerdict, downgraded []model.Finding) {
	for _, d := range downgraded {
		for i := range verdicts {
			v := &verdicts[i]
			if v.Status == VerdictVerified && v.File == d.File && v.Line == d.Line && v.OldLine == d.OldLine {
				v.Status = VerdictDowngraded
				break
			}
		}
	}
}

// describeEmptyResponse explains why a response carried no text, using the
// finish reason or the prompt block reason the API reported.
func describeEmptyResponse(resp *genai.GenerateContentResponse) string {
	if resp == nil {
		return "no response"
	}
	if len(resp.Candidates) == 0 {
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			return "prompt blocked: " + string(resp.PromptFeedback.BlockReason)
		}
		return "no candidates"
	}
	c := resp.Candidates[0]
	detail := "finish reason " + string(c.FinishReason)
	if c.FinishReason == "" {
		detail = "no finish reason"
	}
	if c.FinishMessage != "" {
		detail += ": " + c.FinishMessage
	}
	return detail
}

// extractResponseText pulls text from a genai response.
func extractResponseText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 {
		return ""
	}
	candidate := resp.Candidates[0]
	if candidate.Content == nil || len(candidate.Content.Parts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			sb.WriteString(part.Text)
		}
	}
	return sb.String()
}

// toolResultJSON encodes a tool result for the conversation. HTML escaping is
// off so the <untrusted_tool_output> frame reaches the model as the literal
// tag the prompt names, not as \u003c escapes.
func toolResultJSON(res ToolResult) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return `{"error":"tool result could not be encoded"}`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
