package model

import "strings"

// ReviewResult holds the parsed model response.
type ReviewResult struct {
	Summary   string      `json:"summary"`
	Findings  []Finding   `json:"findings"`
	Usage     *TokenUsage `json:"usage,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	// Digest is what a chunk review reports about its own part of the change,
	// when it was asked to. The reviewer reads it and does not output it.
	Digest *ChunkDigest `json:"digest,omitempty"`

	// Chunks is the number of chunks the review ran in, and ChunksSplit how
	// many of them were cut in halves and reviewed again after a failed or
	// truncated model call. FilesSkipped lists the files of the change that
	// were not reviewed, with the reason. The reviewer fills all three in its
	// report; a model's own answer leaves them empty.
	Chunks       int           `json:"chunks,omitempty"`
	ChunksSplit  int           `json:"chunks_split,omitempty"`
	FilesSkipped []SkippedFile `json:"files_skipped_detail,omitempty"`
}

// SkippedFile is a file that was part of the change but never reviewed.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Finding is a single review comment from the model.
type Finding struct {
	File         string `json:"file"`
	Line         int    `json:"line"`
	OldLine      int    `json:"old_line,omitempty"` // Pre-change line number of a removed line; set only when Line is 0.
	EndLine      int    `json:"end_line,omitempty"`
	Severity     string `json:"severity"`
	Category     string `json:"category"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Suggestion   string `json:"suggestion,omitempty"`
	CodeSnippet  string `json:"code_snippet,omitempty"`
	ExistingCode string `json:"existing_code,omitempty"`
	RuleName     string `json:"rule_name,omitempty"`
	RuleSource   string `json:"rule_source,omitempty"`  // "platform" or "repo"
	RuleFile     string `json:"rule_file,omitempty"`    // e.g. "hipaa-phi.yaml"
	RuleURL      string `json:"rule_url,omitempty"`     // documentation/runbook link
	Fingerprint  string `json:"fingerprint,omitempty"`  // set by the reviewer; see reviewer.Fingerprint
	Anchor       string `json:"anchor,omitempty"`       // set by the reviewer; hash of the anchored lines
	AnchorLines  int    `json:"anchor_lines,omitempty"` // set by the reviewer; lines the anchor spans
	Origin       string `json:"origin,omitempty"`       // set by the reviewer; empty for a finding from the review pass, OriginAgentExplore for one the agent loop added
}

// TokenUsage tracks token consumption across model calls.
//
// CachedInputTokens is a subset of InputTokens: the part of the prompt the
// provider served from a cache, which providers usually bill at a discount.
// InputTokens still counts every prompt token, so a consumer that ignores the
// field sees the same numbers as before.
//
// ThinkingTokens counts the reasoning ("thinking") tokens the provider
// reports. Where it sits in the other counts follows the transport: a Vertex
// AI response reports it apart from OutputTokens and includes it in
// TotalTokens, while an OpenAI-compatible response reports it as part of
// OutputTokens. Both fields are zero when the provider does not report them.
type TokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	ThinkingTokens    int64 `json:"thinking_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
}

// Add accumulates other into u. A nil other adds nothing.
func (u *TokenUsage) Add(other *TokenUsage) {
	if other == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.OutputTokens += other.OutputTokens
	u.ThinkingTokens += other.ThinkingTokens
	u.TotalTokens += other.TotalTokens
}

// Limits on a ChunkDigest, applied by Bounded.
const (
	// MaxDigestEntries is the most entries kept in each list of a digest.
	MaxDigestEntries = 15
	// MaxDigestEntryChars is the longest entry kept, in characters.
	MaxDigestEntryChars = 160
)

// ChunkDigest summarises what one chunk of a review changes, for the step that
// looks for problems across chunks: public symbols added, removed and changed,
// and the contracts (interfaces, schemas, formats, behaviour) the chunk
// touches.
type ChunkDigest struct {
	// Files are the files of the chunk; the reviewer sets them.
	Files            []string `json:"files,omitempty"`
	SymbolsAdded     []string `json:"symbols_added,omitempty"`
	SymbolsRemoved   []string `json:"symbols_removed,omitempty"`
	SymbolsChanged   []string `json:"symbols_changed,omitempty"`
	ContractsTouched []string `json:"contracts_touched,omitempty"`
}

// Bounded returns a copy of d with every list cut to MaxDigestEntries entries
// and every entry flattened to one line of at most MaxDigestEntryChars
// characters, so a chatty or hostile model cannot grow the next prompt.
func (d ChunkDigest) Bounded() ChunkDigest {
	bound := func(in []string) []string {
		var out []string
		for _, s := range in {
			s = strings.Join(strings.Fields(s), " ")
			if s == "" {
				continue
			}
			if r := []rune(s); len(r) > MaxDigestEntryChars {
				s = string(r[:MaxDigestEntryChars]) + "…"
			}
			out = append(out, s)
			if len(out) == MaxDigestEntries {
				break
			}
		}
		return out
	}
	return ChunkDigest{
		Files:            d.Files,
		SymbolsAdded:     bound(d.SymbolsAdded),
		SymbolsRemoved:   bound(d.SymbolsRemoved),
		SymbolsChanged:   bound(d.SymbolsChanged),
		ContractsTouched: bound(d.ContractsTouched),
	}
}

// Empty reports whether the digest lists nothing.
func (d ChunkDigest) Empty() bool {
	return len(d.SymbolsAdded)+len(d.SymbolsRemoved)+len(d.SymbolsChanged)+len(d.ContractsTouched) == 0
}
