package diff

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ChunkStrategy defines how large diffs are split to fit within model context windows.
// This interface is intentionally minimal to allow complex future implementations.
type ChunkStrategy interface {
	// Chunk splits a set of file diffs into groups that each fit within tokenLimit.
	// Returns an error if the strategy cannot produce valid chunks (e.g., FailStrategy
	// when the diff is too large).
	Chunk(diffs []FileDiff, tokenLimit int) ([][]FileDiff, error)
}

// FailStrategy checks whether the diff fits and returns an error if it doesn't.
// This is the default, safe strategy that forces teams to scope MRs properly.
type FailStrategy struct{}

// Chunk returns all diffs in a single chunk if they fit, or an error if they don't.
func (s *FailStrategy) Chunk(diffs []FileDiff, tokenLimit int) ([][]FileDiff, error) {
	total := EstimateTokens(diffs)
	if total > tokenLimit {
		return nil, &DiffTooLargeError{
			EstimatedTokens: total,
			TokenLimit:      tokenLimit,
			FileCount:       len(diffs),
		}
	}
	return [][]FileDiff{diffs}, nil
}

// splitLimitFraction is the share of the context window that SplitStrategy fills
// with diff content; the rest is left for the system prompt and the response.
const splitLimitFraction = 0.80

// SplitStrategy splits diffs into groups that each fit within the token limit.
// Files are packed by directory, so the files of a package, with its tests,
// are reviewed together.
type SplitStrategy struct {
	// TargetTokens, when positive, is the size each group is packed to instead
	// of 80% of the token limit. It is capped at that 80% so a group never
	// outgrows the context window. A file larger than the target still gets a
	// group of its own.
	TargetTokens int
}

// Chunk splits diffs into groups that fit within tokenLimit.
//
// The files form a tree by directory. A directory whose whole subtree fits the
// target is one unit; a larger one keeps its own files apart from its
// subdirectories, and its own files are packed largest first into the first
// group with room. Units are then laid out in path order and joined in that
// order up to the target, so small directories end up with their neighbours
// under the same parent. A source file and its tests are kept together
// wherever they are packed (see packageDir and stemOf). The result depends on
// the set of files only; the groups are ordered by the position of their
// first file in diffs, so a priority order of the input decides what is
// reviewed first. A file larger than the target is cut at hunk boundaries
// into parts (see SplitFile) that are packed as files of their own; a hunk
// larger than the target is a group of its own.
func (s *SplitStrategy) Chunk(diffs []FileDiff, tokenLimit int) ([][]FileDiff, error) {
	if len(diffs) == 0 {
		return nil, nil
	}

	// Reserve ~20% of the limit for the system prompt and response.
	effectiveLimit := int(float64(tokenLimit) * splitLimitFraction)
	if effectiveLimit <= 0 {
		effectiveLimit = tokenLimit
	}
	if s.TargetTokens > 0 && s.TargetTokens < effectiveLimit {
		effectiveLimit = s.TargetTokens
	}

	root := &dirNode{}
	for i := range diffs {
		path := diffs[i].NewPath
		if path == "" {
			path = diffs[i].OldPath
		}
		dir := packageDir(path)
		parts := SplitFile(diffs[i], effectiveLimit)
		if parts == nil {
			root.insert(dir, &packItem{diff: diffs[i], index: i, path: path, tokens: estimateFileDiffTokens(&diffs[i])})
			continue
		}
		for k := range parts {
			root.insert(dir, &packItem{diff: parts[k], index: i, part: k + 1, path: path, tokens: estimateFileDiffTokens(&parts[k])})
		}
	}
	root.total()

	// Join the groups, in path order, while they fit.
	var chunks [][]*packItem
	var current []*packItem
	currentTokens := 0
	for _, g := range root.groups(effectiveLimit) {
		tokens := 0
		for _, it := range g {
			tokens += it.tokens
		}
		if currentTokens+tokens > effectiveLimit && len(current) > 0 {
			chunks = append(chunks, current)
			current, currentTokens = nil, 0
		}
		current = append(current, g...)
		currentTokens += tokens
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	// Review first what the input listed first; the parts of one file in order.
	first := func(c []*packItem) [2]int {
		m := [2]int{c[0].index, c[0].part}
		for _, it := range c {
			if k := [2]int{it.index, it.part}; k[0] < m[0] || (k[0] == m[0] && k[1] < m[1]) {
				m = k
			}
		}
		return m
	}
	sort.SliceStable(chunks, func(i, j int) bool {
		a, b := first(chunks[i]), first(chunks[j])
		return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1])
	})

	out := make([][]FileDiff, len(chunks))
	for i, c := range chunks {
		out[i] = make([]FileDiff, len(c))
		for j, it := range c {
			out[i][j] = it.diff
		}
	}
	return out, nil
}

// packItem is one file in the packing tree.
type packItem struct {
	diff   FileDiff
	index  int // Position in the input.
	part   int // 1-based part number of a file cut by hunks; 0 for a whole file.
	path   string
	tokens int
}

// dirNode is a directory of the packing tree.
type dirNode struct {
	own      []*packItem
	children map[string]*dirNode
	sum      int // Estimated tokens of the whole subtree.
}

// insert adds item under the directory path (slash-separated, "" for the root).
func (n *dirNode) insert(dir string, item *packItem) {
	node := n
	if dir != "" {
		for _, seg := range strings.Split(dir, "/") {
			if node.children == nil {
				node.children = make(map[string]*dirNode)
			}
			child, ok := node.children[seg]
			if !ok {
				child = &dirNode{}
				node.children[seg] = child
			}
			node = child
		}
	}
	node.own = append(node.own, item)
}

// total computes the subtree sizes.
func (n *dirNode) total() int {
	n.sum = 0
	for _, it := range n.own {
		n.sum += it.tokens
	}
	for _, c := range n.children {
		n.sum += c.total()
	}
	return n.sum
}

// sortedChildren returns the subdirectories in name order.
func (n *dirNode) sortedChildren() []*dirNode {
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*dirNode, len(names))
	for i, name := range names {
		out[i] = n.children[name]
	}
	return out
}

// items returns every file of the subtree in path order.
func (n *dirNode) items() []*packItem {
	out := append([]*packItem(nil), n.own...)
	sortItems(out)
	for _, c := range n.sortedChildren() {
		out = append(out, c.items()...)
	}
	return out
}

// groups returns the units of the subtree in path order: the whole subtree
// when it fits limit, otherwise the packed own files followed by the units of
// each subdirectory.
func (n *dirNode) groups(limit int) [][]*packItem {
	if n.sum <= limit {
		if items := n.items(); len(items) > 0 {
			return [][]*packItem{items}
		}
		return nil
	}
	out := packOwn(n.own, limit)
	for _, c := range n.sortedChildren() {
		out = append(out, c.groups(limit)...)
	}
	return out
}

// sortItems orders files by path, which keeps a source file next to its tests
// when they share a directory, and by input position for equal paths.
func sortItems(items []*packItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].path != items[j].path {
			return items[i].path < items[j].path
		}
		if items[i].index != items[j].index {
			return items[i].index < items[j].index
		}
		return items[i].part < items[j].part
	})
}

// packOwn packs the files of one directory into groups of at most limit. A
// source file and its tests form one unit. Units are placed largest first into
// the first group with room, ties in path order; a unit larger than limit gets a
// group of its own.
func packOwn(own []*packItem, limit int) [][]*packItem {
	byStem := make(map[string][]*packItem)
	var stems []string
	for _, it := range own {
		// The parts of a file are units of their own.
		stem := stemOf(it.path)
		if it.part > 0 {
			stem += "\x00" + strconv.Itoa(it.part)
		}
		if _, ok := byStem[stem]; !ok {
			stems = append(stems, stem)
		}
		byStem[stem] = append(byStem[stem], it)
	}
	type unit struct {
		items  []*packItem
		tokens int
	}
	units := make([]unit, 0, len(stems))
	for _, stem := range stems {
		u := unit{items: byStem[stem]}
		sortItems(u.items)
		for _, it := range u.items {
			u.tokens += it.tokens
		}
		units = append(units, u)
	}
	sort.SliceStable(units, func(i, j int) bool {
		if units[i].tokens != units[j].tokens {
			return units[i].tokens > units[j].tokens
		}
		if a, b := units[i].items[0], units[j].items[0]; a.path != b.path {
			return a.path < b.path
		}
		return units[i].items[0].part < units[j].items[0].part
	})

	type bin struct {
		items  []*packItem
		tokens int
	}
	var bins []*bin
	for _, u := range units {
		var target *bin
		for _, b := range bins {
			if b.tokens+u.tokens <= limit {
				target = b
				break
			}
		}
		if target == nil {
			target = &bin{}
			bins = append(bins, target)
		}
		target.items = append(target.items, u.items...)
		target.tokens += u.tokens
	}

	out := make([][]*packItem, len(bins))
	for i, b := range bins {
		sortItems(b.items)
		out[i] = b.items
	}
	// Keep a stable order between the groups of one directory.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i][0].path != out[j][0].path {
			return out[i][0].path < out[j][0].path
		}
		return out[i][0].part < out[j][0].part
	})
	return out
}

// packageDir returns the directory a file is packed under: its own directory
// with the segments that only say "tests" removed, so a package and its tests
// meet in one node. It drops the directories named test, tests, spec, specs and
// __tests__, and the main and test directories of a src layout.
func packageDir(path string) string {
	dir := filepath.ToSlash(filepath.Dir(filepath.ToSlash(path)))
	if dir == "." || dir == "/" {
		return ""
	}
	segs := strings.Split(strings.Trim(dir, "/"), "/")
	kept := make([]string, 0, len(segs))
	for i, seg := range segs {
		switch seg {
		case "test", "tests", "spec", "specs", "__tests__":
			continue
		case "main":
			if i > 0 && segs[i-1] == "src" {
				continue
			}
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/")
}

// testNameSuffixes and testNamePrefixes mark a file name as a test of the file
// with the rest of the name.
var (
	testNameSuffixes = []string{"_test", ".test", "_spec", ".spec", "Test", "Tests"}
	testNamePrefixes = []string{"test_"}
)

// stemOf returns the file name without its extension and without a test marker
// (foo_test.go, foo.test.ts, FooTest.java and test_foo.py all give foo or Foo),
// so a file and its tests share a stem.
func stemOf(path string) string {
	name := filepath.Base(filepath.ToSlash(path))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	for _, suffix := range testNameSuffixes {
		if trimmed := strings.TrimSuffix(name, suffix); trimmed != name && trimmed != "" {
			name = trimmed
			break
		}
	}
	for _, prefix := range testNamePrefixes {
		if trimmed := strings.TrimPrefix(name, prefix); trimmed != name && trimmed != "" {
			name = trimmed
			break
		}
	}
	return name
}

// DiffTooLargeError is returned by FailStrategy when the diff exceeds the model's context window.
type DiffTooLargeError struct {
	EstimatedTokens int
	TokenLimit      int
	FileCount       int
}

func (e *DiffTooLargeError) Error() string {
	return fmt.Sprintf(
		"MR diff too large for model context window\n"+
			"  Estimated: ~%d tokens\n"+
			"  Model limit: %d tokens\n"+
			"  Files: %d\n\n"+
			"Options:\n"+
			"  --chunk-strategy split     Auto-split into review chunks\n"+
			"  --token-limit <tokens>     Override the context window limit\n"+
			"  --model gemini-3.8-flash   Use a model with larger context (1M tokens)\n"+
			"  --excluded-patterns \"...\"  Exclude generated/vendored files",
		e.EstimatedTokens, e.TokenLimit, e.FileCount,
	)
}

// EstimateTokens returns a rough token count for a set of file diffs.
// Uses the approximation of 1 token ≈ 4 characters.
func EstimateTokens(diffs []FileDiff) int {
	total := 0
	for _, d := range diffs {
		total += estimateFileDiffTokens(&d)
	}
	return total
}

func estimateFileDiffTokens(d *FileDiff) int {
	chars := len(d.OldPath) + len(d.NewPath)
	for i := range d.Hunks {
		chars += hunkChars(&d.Hunks[i])
	}
	return chars / 4 // ~4 chars per token
}

// hunkChars is the size of a hunk in characters, as estimateFileDiffTokens
// counts it.
func hunkChars(h *Hunk) int {
	chars := len(h.Header)
	for _, l := range h.Lines {
		chars += len(l.Content) + 2 // +2 for prefix and newline
	}
	return chars
}

// ModelTokenLimits maps known model names to their approximate context window sizes.
// These are conservative estimates leaving room for the system prompt and response.
var ModelTokenLimits = map[string]int{
	// Gemini 4 (1,048,576-token context window).
	"gemini-4-argon":    1000000,
	"gemini-4-carbon":   1000000,
	"gemini-4-neon":     1000000,
	"gemini-4-hydrogen": 1000000,

	// Gemini 3.x (1,048,576-token context window).
	"gemini-3.8-flash":       1000000,
	"gemini-3.7-flash":       1000000,
	"gemini-3.6-flash":       1000000,
	"gemini-3.5-flash":       1000000,
	"gemini-3.5-flash-lite":  1000000,
	"gemini-3.1-flash-lite":  1000000,
	"gemini-3.1-pro-preview": 1000000,
	"gemini-3-flash-preview": 1000000,

	// Gemini 2.5.
	"gemini-2.5-flash": 1000000,
	"gemini-2.5-pro":   1000000,

	// Claude. Vertex AI uses the same IDs; dated snapshots add an "@<date>" suffix.
	"claude-fable-5-1":  1000000,
	"claude-fable-5":    1000000,
	"claude-opus-5":     1000000,
	"claude-opus-4-8":   1000000,
	"claude-opus-4-7":   1000000,
	"claude-opus-4-6":   1000000,
	"claude-sonnet-5":   1000000,
	"claude-sonnet-4-6": 1000000,
	"claude-haiku-4-5":  200000,

	// Earlier names, kept so existing configs resolve the same limit.
	"claude-sonnet-4":   200000,
	"claude-sonnet-4.5": 200000,
	"claude-opus-4":     200000,
	"claude-haiku-4.5":  200000,

	"mistral-medium-3": 128000,
}

// TokenLimitForModel returns the token limit for a model, or a conservative default.
// A Vertex AI snapshot suffix such as "claude-haiku-4-5@20251001" resolves to its base model.
func TokenLimitForModel(model string) int {
	if limit, ok := ModelTokenLimits[model]; ok {
		return limit
	}
	if base, _, found := strings.Cut(model, "@"); found {
		if limit, ok := ModelTokenLimits[base]; ok {
			return limit
		}
	}
	// Conservative default for unknown models.
	return 128000
}

// NewChunkStrategy creates a ChunkStrategy from a strategy name.
func NewChunkStrategy(name string) (ChunkStrategy, error) {
	return NewChunkStrategyWithTarget(name, 0)
}

// NewChunkStrategyWithTarget is NewChunkStrategy with a chunk size target in
// estimated tokens. The target only applies to the split strategy; 0 keeps the
// default of 80% of the token limit.
func NewChunkStrategyWithTarget(name string, targetTokens int) (ChunkStrategy, error) {
	switch name {
	case "fail", "":
		return &FailStrategy{}, nil
	case "split":
		return &SplitStrategy{TargetTokens: targetTokens}, nil
	default:
		return nil, fmt.Errorf("unknown chunk strategy: %q (valid: fail, split)", name)
	}
}

// SplitChunk cuts a chunk into two parts of about equal estimated size, in
// input order, so files that were packed together stay together. Neither part
// is empty. A chunk of one file is cut at a hunk boundary into two parts of
// that file (see SplitFile). It returns nil for a chunk that cannot be cut: no
// files, or a single file of one hunk.
func SplitChunk(chunk []FileDiff) [][]FileDiff {
	if len(chunk) == 1 {
		halves := splitFileInHalf(chunk[0])
		if halves == nil {
			return nil
		}
		return [][]FileDiff{{halves[0]}, {halves[1]}}
	}
	if len(chunk) < 2 {
		return nil
	}
	total := EstimateTokens(chunk)
	cut, running := 1, 0
	for i := range chunk[:len(chunk)-1] {
		running += estimateFileDiffTokens(&chunk[i])
		cut = i + 1
		if running*2 >= total {
			break
		}
	}
	return [][]FileDiff{chunk[:cut:cut], chunk[cut:]}
}
