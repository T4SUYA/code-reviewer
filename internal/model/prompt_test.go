package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPrompt_AllFocus(t *testing.T) {
	prompt := BuildPrompt([]string{"all"}, "")
	if !strings.Contains(prompt, "PERSONA") {
		t.Error("prompt missing base PERSONA section")
	}
	if !strings.Contains(prompt, "Bug Detection") {
		t.Error("prompt missing bugs focus")
	}
	if !strings.Contains(prompt, "Security Review") {
		t.Error("prompt missing security focus")
	}
	if !strings.Contains(prompt, "Performance Review") {
		t.Error("prompt missing performance focus")
	}
}

func TestBuildPrompt_SingleFocus(t *testing.T) {
	prompt := BuildPrompt([]string{"security"}, "")
	if !strings.Contains(prompt, "Security Review") {
		t.Error("prompt missing security focus")
	}
	if strings.Contains(prompt, "Bug Detection") {
		t.Error("prompt should not contain bugs focus when only security is selected")
	}
}

func TestBuildPrompt_MultipleFocus(t *testing.T) {
	prompt := BuildPrompt([]string{"bugs", "performance"}, "")
	if !strings.Contains(prompt, "Bug Detection") {
		t.Error("prompt missing bugs focus")
	}
	if !strings.Contains(prompt, "Performance Review") {
		t.Error("prompt missing performance focus")
	}
	if strings.Contains(prompt, "Security Review") {
		t.Error("prompt should not contain security focus")
	}
}

func TestBuildPrompt_ExtraRules(t *testing.T) {
	rules := "Always flag raw SQL queries."
	prompt := BuildPrompt([]string{"all"}, rules)
	if !strings.Contains(prompt, "ADDITIONAL RULES") {
		t.Error("prompt missing ADDITIONAL RULES section")
	}
	if !strings.Contains(prompt, rules) {
		t.Error("prompt missing custom rules content")
	}
}

func TestBuildPrompt_NoExtraRules(t *testing.T) {
	prompt := BuildPrompt([]string{"all"}, "")
	if strings.Contains(prompt, "ADDITIONAL RULES") {
		t.Error("prompt should not contain ADDITIONAL RULES when none provided")
	}
}

func TestBuildPrompt_EmptyFocus(t *testing.T) {
	// Empty focus should default to all.
	prompt := BuildPrompt(nil, "")
	if !strings.Contains(prompt, "Bug Detection") {
		t.Error("empty focus should include all overlays")
	}
}

func TestBuildUserPrompt(t *testing.T) {
	prompt := BuildUserPrompt("Fix nil deref", "Handles nil response", "+ fixed line")
	if !strings.Contains(prompt, "Fix nil deref") {
		t.Error("user prompt missing MR title")
	}
	if !strings.Contains(prompt, "Handles nil response") {
		t.Error("user prompt missing MR description")
	}
	if !strings.Contains(prompt, "```diff") {
		t.Error("user prompt missing diff block")
	}
}

func TestBuildUserPrompt_NoMetadata(t *testing.T) {
	prompt := BuildUserPrompt("", "", "+ some code")
	if strings.Contains(prompt, "Merge Request:") {
		t.Error("should not include MR title header when empty")
	}
	if !strings.Contains(prompt, "```diff") {
		t.Error("user prompt missing diff block")
	}
}

func TestBuildPromptWithCustom_ValidFile(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(promptFile, []byte("You are a Go specialist."), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := BuildPromptWithCustom(promptFile, []string{"security"}, "Flag raw SQL.")
	if !strings.Contains(prompt, "You are a Go specialist.") {
		t.Error("prompt should contain custom prompt content")
	}
	if strings.Contains(prompt, "Principal Software Engineer") {
		t.Error("prompt should NOT contain built-in base prompt when custom is provided")
	}
	if !strings.Contains(prompt, "Security Review") {
		t.Error("focus overlays should still be appended to custom prompt")
	}
	if !strings.Contains(prompt, "Flag raw SQL.") {
		t.Error("extra rules should still be appended to custom prompt")
	}
}

func TestBuildPromptWithCustom_MissingFile(t *testing.T) {
	prompt := BuildPromptWithCustom("/nonexistent/path/prompt.md", []string{"all"}, "")
	// Should fall back to built-in prompt.
	if !strings.Contains(prompt, "Principal Software Engineer") {
		t.Error("missing custom prompt file should fall back to built-in base prompt")
	}
}

func TestBuildPromptWithCustom_EmptyPath(t *testing.T) {
	prompt := BuildPromptWithCustom("", []string{"bugs"}, "")
	// Empty path = use built-in prompt (same as BuildPrompt).
	if !strings.Contains(prompt, "Principal Software Engineer") {
		t.Error("empty custom prompt path should use built-in base prompt")
	}
	if !strings.Contains(prompt, "Bug Detection") {
		t.Error("focus overlays should be applied")
	}
}

func TestBuildPromptFull_ReviewMD(t *testing.T) {
	reviewMD := "## Always check\n- New API routes have integration tests\n- No PII in logs"
	prompt := BuildPromptFull("", reviewMD, []string{"bugs"}, "", "")

	if !strings.Contains(prompt, "REVIEW INSTRUCTIONS (HIGHEST PRIORITY)") {
		t.Error("prompt should contain REVIEW INSTRUCTIONS header")
	}
	if !strings.Contains(prompt, "New API routes have integration tests") {
		t.Error("prompt should contain REVIEW.md content")
	}
	if !strings.Contains(prompt, "No PII in logs") {
		t.Error("prompt should contain all REVIEW.md content")
	}
	// Verify REVIEW.md comes after base prompt and focus overlays.
	baseIdx := strings.Index(prompt, "Principal Software Engineer")
	reviewIdx := strings.Index(prompt, "REVIEW INSTRUCTIONS")
	if reviewIdx <= baseIdx {
		t.Error("REVIEW.md should appear after base prompt (recency = highest priority)")
	}
	// Verify immutable guardrails come after REVIEW.md.
	guardrailIdx := strings.Index(prompt, "IMMUTABLE OUTPUT CONSTRAINTS")
	if guardrailIdx < 0 {
		t.Fatal("prompt should contain IMMUTABLE OUTPUT CONSTRAINTS")
	}
	if guardrailIdx <= reviewIdx {
		t.Error("immutable guardrails should appear after REVIEW.md")
	}
}

func TestBuildPromptFull_ReviewMD_AfterExtraRules(t *testing.T) {
	reviewMD := "Only report CRITICAL severity."
	prompt := BuildPromptFull("", reviewMD, []string{"all"}, "Flag raw SQL.", "")

	rulesIdx := strings.Index(prompt, "ADDITIONAL RULES")
	reviewIdx := strings.Index(prompt, "REVIEW INSTRUCTIONS")
	if rulesIdx < 0 || reviewIdx < 0 {
		t.Fatal("both ADDITIONAL RULES and REVIEW INSTRUCTIONS should be present")
	}
	if reviewIdx <= rulesIdx {
		t.Error("REVIEW.md should appear after extra rules (highest priority = last)")
	}
}

func TestBuildPromptFull_EmptyReviewMD(t *testing.T) {
	prompt := BuildPromptFull("", "", []string{"bugs"}, "", "")
	if strings.Contains(prompt, "REVIEW INSTRUCTIONS") {
		t.Error("empty reviewMD should not inject REVIEW INSTRUCTIONS section")
	}
}

func TestBuildPromptFull_WithCustomPromptAndReviewMD(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(promptFile, []byte("You are a security auditor."), 0o644); err != nil {
		t.Fatal(err)
	}

	reviewMD := "Focus on SQL injection only."
	prompt := BuildPromptFull(promptFile, reviewMD, []string{"security"}, "", "")

	if !strings.Contains(prompt, "You are a security auditor.") {
		t.Error("custom prompt should be used as base")
	}
	if !strings.Contains(prompt, "Focus on SQL injection only.") {
		t.Error("REVIEW.md should be appended")
	}
	if strings.Contains(prompt, "Principal Software Engineer") {
		t.Error("built-in base should not be present when custom prompt is used")
	}
}

func TestBuildUserPromptWithContext_WithSnippets(t *testing.T) {
	snippets := []ContextSnippet{
		{File: "handler.go", Line: 42, Content: "auth.ValidateSession(token)", Symbol: "ValidateSession"},
		{File: "middleware.go", Line: 18, Content: "auth.ValidateSession(req.Token)", Symbol: "ValidateSession"},
	}

	prompt := BuildUserPromptWithContext("Fix auth", "Updated session validation", "diff content", snippets)

	if !strings.Contains(prompt, "Related Unchanged Code") {
		t.Error("should contain Related Unchanged Code section")
	}
	if !strings.Contains(prompt, "handler.go:42") {
		t.Error("should contain handler.go:42 reference")
	}
	if !strings.Contains(prompt, "middleware.go:18") {
		t.Error("should contain middleware.go:18 reference")
	}
	if !strings.Contains(prompt, "`ValidateSession`") {
		t.Error("should contain symbol name")
	}
	if !strings.Contains(prompt, "auth.ValidateSession(token)") {
		t.Error("should contain snippet content")
	}
}

func TestBuildUserPromptWithContext_NoSnippets(t *testing.T) {
	prompt := BuildUserPromptWithContext("Fix auth", "desc", "diff content", nil)
	if strings.Contains(prompt, "Related Unchanged Code") {
		t.Error("should not contain Related Unchanged Code when no snippets")
	}
	// Should still contain the normal diff.
	if !strings.Contains(prompt, "diff content") {
		t.Error("should contain diff content")
	}
}

func TestBuildUserPromptWithContext_EmptySnippets(t *testing.T) {
	prompt := BuildUserPromptWithContext("Fix auth", "desc", "diff content", []ContextSnippet{})
	if strings.Contains(prompt, "Related Unchanged Code") {
		t.Error("empty snippets should not inject the Related Unchanged Code section")
	}
}

// TestBasePrompt_PrecisionRules verifies that the base prompt contains all
// quality-improvement rules added to reduce false positives and improve precision.
func TestBasePrompt_PrecisionRules(t *testing.T) {
	prompt := BuildPrompt(nil, "")

	checks := []struct {
		name    string
		substr  string
	}{
		{"precision penalty", "false positive wastes more developer time"},
		{"confidence threshold", "confidence below 80%"},
		{"zero is fine", "empty findings array is a perfectly valid review"},
		{"no manufacturing", "Do not manufacture findings"},
		{"focus on additions", "FOCUS ON ADDITIONS"},
		{"additions detail", "Concentrate on lines starting with '+'"},
		{"removed code in scope", "Removed code ('-') is in scope only when the removal itself introduces a defect"},
		{"scope restriction", `Use "scope" ONLY for intent-driven findings`},
		{"no split suggestion", "split into smaller changes"},
		{"suggestion import guard", "references variables, functions, or imports not visible"},
		{"medium tightened", "NOT: style preferences"},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(prompt, c.substr) {
				t.Errorf("base prompt should contain %q", c.substr)
			}
		})
	}
}

func TestBuildPromptWithPlatform(t *testing.T) {
	platformMD := "## Platform HIPAA Rules\n- Mask all patient MRNs"
	repoMD := "## Service Rules\n- Use service helper"
	prompt := BuildPromptWithPlatform("", platformMD, repoMD, []string{"security"}, "extra", "intent")

	if !strings.Contains(prompt, "MANDATORY PLATFORM REQUIREMENTS (HIGHEST PRIORITY - NON-NEGOTIABLE)") {
		t.Error("expected mandatory platform requirements header")
	}
	if !strings.Contains(prompt, "Mask all patient MRNs") {
		t.Error("expected platform mandate content")
	}
	if !strings.Contains(prompt, "REPOSITORY REVIEW INSTRUCTIONS") {
		t.Error("expected repository review instructions header")
	}
	if !strings.Contains(prompt, "Use service helper") {
		t.Error("expected repo review content")
	}

	// Verify platform requirements appear AFTER repo instructions for recency bias
	repoIdx := strings.Index(prompt, "REPOSITORY REVIEW INSTRUCTIONS")
	platIdx := strings.Index(prompt, "MANDATORY PLATFORM REQUIREMENTS")
	if platIdx <= repoIdx {
		t.Errorf("platform requirements should appear after repo instructions for recency bias, got plat=%d repo=%d", platIdx, repoIdx)
	}

	// Verify immutable guardrails appear after platform mandates
	guardIdx := strings.Index(prompt, "IMMUTABLE OUTPUT CONSTRAINTS")
	if guardIdx <= platIdx {
		t.Errorf("immutable guardrails should appear after platform mandates, got guard=%d plat=%d", guardIdx, platIdx)
	}
}

func TestBuildPromptWithPlatform_RulesWithoutPlatformMD(t *testing.T) {
	extraRules := "## MANDATORY PLATFORM COMPLIANCE RULES\n\n### [Platform Mandate] mask-phi\n..."
	repoMD := "## Service Rules\n- Use service helper"
	prompt := BuildPromptWithPlatform("", "", repoMD, []string{"security"}, extraRules, "")

	if strings.Contains(prompt, "REVIEW INSTRUCTIONS (HIGHEST PRIORITY)") {
		t.Error("REVIEW.md should not claim HIGHEST PRIORITY when mandatory platform rules exist in extraRules")
	}
	if !strings.Contains(prompt, "REPOSITORY REVIEW INSTRUCTIONS") {
		t.Error("expected REPOSITORY REVIEW INSTRUCTIONS header")
	}
}

func TestWithConfidenceFloor_DefaultIsByteIdentical(t *testing.T) {
	base := BuildPrompt([]string{"all"}, "")
	for _, floor := range []int{0, 80} {
		if got := WithConfidenceFloor(base, floor); got != base {
			t.Errorf("WithConfidenceFloor(base, %d) changed the prompt", floor)
		}
	}
}

func TestWithConfidenceFloor_Custom(t *testing.T) {
	base := BuildPrompt([]string{"all"}, "")
	got := WithConfidenceFloor(base, 95)
	if !strings.Contains(got, "confidence below 95%, omit the finding") {
		t.Errorf("prompt does not carry the 95%% floor")
	}
	if strings.Contains(got, "below 80%") {
		t.Errorf("prompt still carries the default floor")
	}
}

func TestWithConfidenceFloor_CustomPromptWithoutFloorSentenceUnchanged(t *testing.T) {
	const custom = "Review the diff and answer in JSON."
	if got := WithConfidenceFloor(custom, 50); got != custom {
		t.Errorf("custom prompt without a floor sentence was modified: %q", got)
	}
}

func TestBuildPrompt_RemovedCodeIsInScope(t *testing.T) {
	prompt := BuildPrompt([]string{"all"}, "")
	if strings.Contains(prompt, "Do NOT comment on deleted lines") {
		t.Error("prompt still forbids findings on deleted lines")
	}
	for _, want := range []string{`"old_line"`, "removed"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestWithPromptProfile_PrecisionIsByteIdenticalToGolden(t *testing.T) {
	golden, err := os.ReadFile("testdata/prompt_precision.golden")
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	base := BuildPrompt([]string{"all"}, "")
	if base != string(golden) {
		t.Fatal("default prompt differs from the golden precision prompt")
	}
	for _, profile := range []string{"", "precision"} {
		if got := WithPromptProfile(base, profile); got != string(golden) {
			t.Errorf("WithPromptProfile(%q) changed the prompt", profile)
		}
	}
}

func TestWithPromptProfile_ReplacesOnlyTheStanceLine(t *testing.T) {
	base := BuildPrompt([]string{"all"}, "")
	for _, profile := range []string{"balanced", "recall"} {
		t.Run(profile, func(t *testing.T) {
			got := WithPromptProfile(base, profile)
			if got == base {
				t.Fatal("prompt unchanged")
			}
			if strings.Contains(got, "When in doubt, DO NOT flag it") {
				t.Error("precision stance still present")
			}
			want := strings.Replace(base, precisionStance, promptProfileStances[profile], 1)
			want = strings.Replace(want, precisionZeroIsFine, nonPrecisionZeroIsFine, 1)
			if got != want {
				t.Error("prompt differs from the base outside the stance and empty-review lines")
			}
			if !strings.Contains(got, "ZERO IS FINE") || !strings.Contains(got, "## OUTPUT FORMAT") {
				t.Error("surrounding rules were lost")
			}
		})
	}
}

func TestWithPromptProfile_Content(t *testing.T) {
	base := BuildPrompt(nil, "")
	recall := WithPromptProfile(base, "recall")
	for _, want := range []string{"Investigate every suspicious pattern", "honest confidence", "threshold decides what is kept"} {
		if !strings.Contains(recall, want) {
			t.Errorf("recall prompt missing %q", want)
		}
	}
	balanced := WithPromptProfile(base, "balanced")
	if !strings.Contains(balanced, "even if it is not certain") {
		t.Error("balanced prompt missing its stance")
	}
	if strings.Contains(balanced, "Investigate every suspicious pattern") {
		t.Error("balanced prompt carries the recall stance")
	}
}

func TestWithPromptProfile_UnknownAndCustomPromptsUnchanged(t *testing.T) {
	base := BuildPrompt(nil, "")
	if got := WithPromptProfile(base, "bogus"); got != base {
		t.Error("unknown profile changed the prompt")
	}
	custom := "Review the diff and be kind."
	if got := WithPromptProfile(custom, "recall"); got != custom {
		t.Error("custom prompt without the stance line changed")
	}
}

// Every profile keeps the floor clause, so the confidence floor still sets the
// threshold whichever stance is in effect.
func TestWithPromptProfile_ComposesWithConfidenceFloor(t *testing.T) {
	base := BuildPrompt([]string{"bugs"}, "")
	for _, profile := range []string{"", "precision", "balanced", "recall"} {
		got := WithConfidenceFloor(WithPromptProfile(base, profile), 60)
		if !strings.Contains(got, "confidence below 60%, omit the finding") {
			t.Errorf("profile %q: floor not applied", profile)
		}
		if strings.Contains(got, "below 80%") {
			t.Errorf("profile %q: default floor left behind", profile)
		}
	}
}

func TestWithPromptProfile_EmptyReviewLineMatchesStance(t *testing.T) {
	base := BuildPrompt(nil, "")
	if !strings.Contains(base, precisionZeroIsFine) {
		t.Fatal("default prompt lacks the precision empty-review line")
	}
	for _, profile := range []string{"balanced", "recall"} {
		got := WithPromptProfile(base, profile)
		if strings.Contains(got, "perfectly valid review") {
			t.Errorf("%s: precision empty-review wording still present", profile)
		}
		if !strings.Contains(got, "valid only if nothing suspicious remains after you investigated") {
			t.Errorf("%s: reworded empty-review line missing", profile)
		}
	}
	for _, profile := range []string{"", "precision"} {
		if got := WithPromptProfile(base, profile); !strings.Contains(got, precisionZeroIsFine) {
			t.Errorf("%s: precision empty-review line changed", profile)
		}
	}
}
