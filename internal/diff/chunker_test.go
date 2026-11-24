package diff

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func makeTestDiffs(count, linesPerFile int) []FileDiff {
	diffs := make([]FileDiff, count)
	for i := range diffs {
		lines := make([]DiffLine, linesPerFile)
		for j := range lines {
			lines[j] = DiffLine{
				Type:      LineAdded,
				Content:   "func example() { return nil }",
				NewLineNo: j + 1,
			}
		}
		diffs[i] = FileDiff{
			NewPath: "file_" + string(rune('a'+i)) + ".go",
			Hunks: []Hunk{{
				Header:   "@@ -0,0 +1," + string(rune('0'+linesPerFile)) + " @@",
				NewStart: 1,
				NewCount: linesPerFile,
				Lines:    lines,
			}},
		}
	}
	return diffs
}

func TestFailStrategy_WithinLimit(t *testing.T) {
	diffs := makeTestDiffs(3, 10)
	s := &FailStrategy{}
	chunks, err := s.Chunk(diffs, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
	if len(chunks[0]) != 3 {
		t.Errorf("expected 3 files in chunk, got %d", len(chunks[0]))
	}
}

func TestFailStrategy_ExceedsLimit(t *testing.T) {
	diffs := makeTestDiffs(5, 100)
	s := &FailStrategy{}
	_, err := s.Chunk(diffs, 10) // Tiny limit.
	if err == nil {
		t.Fatal("expected error for oversized diff")
	}
	dte, ok := err.(*DiffTooLargeError)
	if !ok {
		t.Fatalf("expected DiffTooLargeError, got %T", err)
	}
	if dte.FileCount != 5 {
		t.Errorf("FileCount = %d, want 5", dte.FileCount)
	}
}

func TestSplitStrategy_SingleChunk(t *testing.T) {
	diffs := makeTestDiffs(3, 10)
	s := &SplitStrategy{}
	chunks, err := s.Chunk(diffs, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for small diff, got %d", len(chunks))
	}
}

func TestSplitStrategy_MultipleChunks(t *testing.T) {
	diffs := makeTestDiffs(10, 100)
	tokens := EstimateTokens(diffs)
	// Set limit to roughly half.
	limit := tokens / 2

	s := &SplitStrategy{}
	chunks, err := s.Chunk(diffs, limit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks, got %d", len(chunks))
	}

	// Verify all files are present.
	totalFiles := 0
	for _, chunk := range chunks {
		totalFiles += len(chunk)
	}
	if totalFiles != 10 {
		t.Errorf("expected 10 total files across chunks, got %d", totalFiles)
	}
}

func TestSplitStrategy_EmptyInput(t *testing.T) {
	s := &SplitStrategy{}
	chunks, err := s.Chunk(nil, 100000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunks != nil {
		t.Errorf("expected nil chunks for empty input, got %v", chunks)
	}
}

func TestEstimateTokens(t *testing.T) {
	diffs := makeTestDiffs(1, 10)
	tokens := EstimateTokens(diffs)
	if tokens <= 0 {
		t.Errorf("expected positive token count, got %d", tokens)
	}
}

func TestTokenLimitForModel(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"gemini-4-argon", 1000000},
		{"gemini-4-carbon", 1000000},
		{"gemini-4-neon", 1000000},
		{"gemini-4-hydrogen", 1000000},
		{"gemini-3.8-flash", 1000000},
		{"gemini-2.5-flash", 1000000},
		{"claude-sonnet-5", 1000000},
		{"claude-haiku-4-5", 200000},
		{"claude-haiku-4-5@20251001", 200000}, // Vertex AI snapshot suffix.
		{"claude-sonnet-4", 200000},
		{"unknown-model", 128000},          // Default.
		{"unknown-model@20251001", 128000}, // Default, suffix or not.
	}
	for _, tt := range tests {
		got := TokenLimitForModel(tt.model)
		if got != tt.want {
			t.Errorf("TokenLimitForModel(%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

func TestNewChunkStrategy(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"fail", false},
		{"split", false},
		{"", false},
		{"invalid", true},
	}
	for _, tt := range tests {
		_, err := NewChunkStrategy(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("NewChunkStrategy(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

// sizedDiff returns a one-line diff for path whose content makes it roughly
// `tokens` estimated tokens.
func sizedDiff(path string, tokens int) FileDiff {
	return FileDiff{
		NewPath: path,
		Hunks: []Hunk{{
			Lines: []DiffLine{{Type: LineAdded, Content: strings.Repeat("x", tokens*4), NewLineNo: 1}},
		}},
	}
}

func chunkPaths(chunks [][]FileDiff) [][]string {
	out := make([][]string, len(chunks))
	for i, c := range chunks {
		for _, d := range c {
			out[i] = append(out[i], d.NewPath)
		}
	}
	return out
}

func TestSplitStrategy_TargetTokensPacksToTarget(t *testing.T) {
	var diffs []FileDiff
	for i := range 10 {
		diffs = append(diffs, sizedDiff(fmt.Sprintf("f%02d.go", i), 100))
	}

	// Default: the 1M limit holds everything in one chunk.
	chunks, err := (&SplitStrategy{}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("default packing: got %d chunks, want 1", len(chunks))
	}

	// A 350-token target holds three 100-token files per chunk.
	chunks, err = (&SplitStrategy{TargetTokens: 350}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4 (3+3+3+1)", len(chunks))
	}
	for i, c := range chunks {
		if got := EstimateTokens(c); got > 350 {
			t.Errorf("chunk %d is ~%d tokens, over the 350 target", i, got)
		}
	}
}

func TestSplitStrategy_TargetTokensAboveDefaultIsCapped(t *testing.T) {
	var diffs []FileDiff
	for i := range 4 {
		diffs = append(diffs, sizedDiff(fmt.Sprintf("f%d.go", i), 300))
	}
	// Limit 1000 leaves 800 for diff content; a target of 5000 must not raise it.
	chunks, err := (&SplitStrategy{TargetTokens: 5000}).Chunk(diffs, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (the 80%% cap of 800 tokens holds two files)", len(chunks))
	}
}

func TestSplitStrategy_FileLargerThanTargetGetsOwnChunk(t *testing.T) {
	diffs := []FileDiff{
		sizedDiff("small1.go", 50),
		sizedDiff("huge.go", 900),
		sizedDiff("small2.go", 50),
	}
	chunks, err := (&SplitStrategy{TargetTokens: 200}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// Chunks follow the input order of their first file; the oversized file is
	// alone in its own.
	want := [][]string{{"small1.go", "small2.go"}, {"huge.go"}}
	if got := chunkPaths(chunks); !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
}

func TestSplitStrategy_DeterministicOrder(t *testing.T) {
	// Equal sizes: the order must follow the input, run after run.
	var diffs []FileDiff
	for i := range 12 {
		diffs = append(diffs, sizedDiff(fmt.Sprintf("f%02d.go", i), 100))
	}
	s := &SplitStrategy{TargetTokens: 250}
	first, err := s.Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"f00.go", "f01.go"}, {"f02.go", "f03.go"}, {"f04.go", "f05.go"},
		{"f06.go", "f07.go"}, {"f08.go", "f09.go"}, {"f10.go", "f11.go"},
	}
	if got := chunkPaths(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %v, want %v", got, want)
	}
	for range 20 {
		again, err := s.Chunk(diffs, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(chunkPaths(again), chunkPaths(first)) {
			t.Fatal("chunking the same diff twice gave different chunks")
		}
	}
}

func TestSplitStrategy_ZeroTargetKeepsDefault(t *testing.T) {
	diffs := makeTestDiffs(10, 100)
	limit := EstimateTokens(diffs) / 2
	a, err := (&SplitStrategy{}).Chunk(diffs, limit)
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&SplitStrategy{TargetTokens: 0}).Chunk(diffs, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chunkPaths(a), chunkPaths(b)) {
		t.Error("TargetTokens 0 changed the chunking")
	}
}

func TestNewChunkStrategyWithTarget(t *testing.T) {
	s, err := NewChunkStrategyWithTarget("split", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*SplitStrategy).TargetTokens; got != 1234 {
		t.Errorf("TargetTokens = %d, want 1234", got)
	}
	if _, err := NewChunkStrategyWithTarget("bogus", 1); err == nil {
		t.Error("expected an error for an unknown strategy")
	}
}

func TestSplitChunk(t *testing.T) {
	sized := func(lines ...int) []FileDiff {
		var ds []FileDiff
		for i, n := range lines {
			d := makeTestDiffs(1, n)[0]
			d.NewPath = fmt.Sprintf("f%d.go", i)
			ds = append(ds, d)
		}
		return ds
	}
	paths := func(ds []FileDiff) string {
		var p []string
		for _, d := range ds {
			p = append(p, d.NewPath)
		}
		return strings.Join(p, ",")
	}

	tests := []struct {
		name  string
		chunk []FileDiff
		want  []string // paths of each half; nil when the chunk cannot be split.
	}{
		{"empty", nil, nil},
		{"one file", sized(10), nil},
		{"two files", sized(10, 10), []string{"f0.go", "f1.go"}},
		{"even files halve by count", sized(10, 10, 10, 10), []string{"f0.go,f1.go", "f2.go,f3.go"}},
		{"halves balance by size", sized(60, 10, 10, 10, 10), []string{"f0.go", "f1.go,f2.go,f3.go,f4.go"}},
		{"neither half is empty", sized(1, 1, 100), []string{"f0.go,f1.go", "f2.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := SplitChunk(tt.chunk)
			if tt.want == nil {
				if parts != nil {
					t.Fatalf("SplitChunk = %v, want nil", parts)
				}
				return
			}
			if len(parts) != len(tt.want) {
				t.Fatalf("got %d parts, want %d", len(parts), len(tt.want))
			}
			for i, w := range tt.want {
				if got := paths(parts[i]); got != w {
					t.Errorf("part %d = %s, want %s", i, got, w)
				}
			}
		})
	}
}

// packed returns the chunks of the files, each a path list, using a target
// that makes a chunk hold about `perChunk` files of 100 tokens.
func packed(t *testing.T, perChunk int, diffs []FileDiff) [][]string {
	t.Helper()
	chunks, err := (&SplitStrategy{TargetTokens: perChunk*100 + 50}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	return chunkPaths(chunks)
}

func sized100(paths ...string) []FileDiff {
	var ds []FileDiff
	for _, p := range paths {
		ds = append(ds, sizedDiff(p, 100))
	}
	return ds
}

func TestSplitStrategy_KeepsDirectoriesTogether(t *testing.T) {
	// Interleaved in the input, as a priority order would leave them.
	diffs := sized100(
		"svc/api/handler.go", "svc/store/db.go", "svc/api/routes.go",
		"svc/store/queries.go", "svc/api/auth.go", "svc/store/tx.go",
	)
	got := packed(t, 3, diffs)
	want := [][]string{
		{"svc/api/auth.go", "svc/api/handler.go", "svc/api/routes.go"},
		{"svc/store/db.go", "svc/store/queries.go", "svc/store/tx.go"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
}

func TestSplitStrategy_SourceAndTestsShareAChunk(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
	}{
		{"go: test beside source", []string{"pkg/a/a.go", "pkg/a/a_test.go", "pkg/b/b.go", "pkg/b/b_test.go"}},
		{"java: src/main and src/test", []string{"mod/src/main/java/app/A.java", "mod/src/test/java/app/ATest.java", "mod/src/main/java/lib/B.java", "mod/src/test/java/lib/BTest.java"}},
		{"python: tests directory", []string{"a/mod.py", "tests/a/test_mod.py", "b/other.py", "tests/b/test_other.py"}},
		{"ts: __tests__ directory", []string{"web/a/x.ts", "web/a/__tests__/x.test.ts", "web/b/y.ts", "web/b/__tests__/y.test.ts"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := packed(t, 2, sized100(tt.paths...))
			if len(got) != 2 {
				t.Fatalf("chunks = %v, want 2 chunks of two", got)
			}
			// Each chunk holds one source file and its test: the same stem.
			for _, c := range got {
				if len(c) != 2 {
					t.Fatalf("chunks = %v, want two files per chunk", got)
				}
				stem := func(p string) string {
					b := p[strings.LastIndex(p, "/")+1:]
					b = strings.TrimSuffix(b, filepath.Ext(b))
					b = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(b, ".test"), "_test"), "Test")
					return strings.ToLower(strings.TrimPrefix(b, "test_"))
				}
				if stem(c[0]) != stem(c[1]) {
					t.Errorf("chunk %v splits a source file from its test", c)
				}
			}
		})
	}
}

func TestSplitStrategy_SmallDirectoriesJoinUnderTheirParent(t *testing.T) {
	// Four directories of one file each, two chunks: neighbours under the same
	// parent land together rather than by size.
	diffs := []FileDiff{
		sizedDiff("x/a/one.go", 100), sizedDiff("y/b/two.go", 100),
		sizedDiff("x/c/three.go", 100), sizedDiff("y/d/four.go", 100),
	}
	got := packed(t, 2, diffs)
	want := [][]string{{"x/a/one.go", "x/c/three.go"}, {"y/b/two.go", "y/d/four.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
}

func TestSplitStrategy_OversizedDirectorySplitsLargestFirst(t *testing.T) {
	diffs := []FileDiff{
		sizedDiff("d/small1.go", 40), sizedDiff("d/big.go", 150),
		sizedDiff("d/small2.go", 40), sizedDiff("d/mid.go", 100),
	}
	chunks, err := (&SplitStrategy{TargetTokens: 200}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	// Largest first, each into the first chunk with room: big+small1 | mid+small2.
	want := [][]string{{"d/big.go", "d/small1.go"}, {"d/mid.go", "d/small2.go"}}
	if got := chunkPaths(chunks); !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
}

func TestSplitStrategy_PackingIsDeterministicWhateverTheInputOrder(t *testing.T) {
	var diffs []FileDiff
	for _, dir := range []string{"a", "a/sub", "b", "c/d", "c/e", "f"} {
		for i := range 3 {
			diffs = append(diffs, sizedDiff(fmt.Sprintf("%s/f%d.go", dir, i), 60+10*i))
		}
	}
	s := &SplitStrategy{TargetTokens: 400}
	members := func(ds []FileDiff) []string {
		chunks, err := s.Chunk(ds, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range chunks {
			var ps []string
			for _, d := range c {
				ps = append(ps, d.NewPath)
			}
			out = append(out, strings.Join(ps, ","))
		}
		sort.Strings(out)
		return out
	}
	want := members(diffs)
	rng := rand.New(rand.NewPCG(1, 2))
	for range 25 {
		shuffled := append([]FileDiff(nil), diffs...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := members(shuffled); !reflect.DeepEqual(got, want) {
			t.Fatalf("the same files in another input order packed differently:\n got %v\nwant %v", got, want)
		}
	}
}

func TestSplitStrategy_ChunksFollowTheInputPriority(t *testing.T) {
	diffs := []FileDiff{sizedDiff("z/secret.go", 100), sizedDiff("a/readme.go", 100), sizedDiff("z/other.go", 100), sizedDiff("a/doc.go", 100)}
	got := packed(t, 2, diffs)
	if !slices.Contains(got[0], "z/secret.go") {
		t.Errorf("chunks = %v, want the chunk of the first input file first", got)
	}
}

func TestSplitStrategy_EveryFileOnceAndChunksFit(t *testing.T) {
	var diffs []FileDiff
	for i := range 60 {
		diffs = append(diffs, sizedDiff(fmt.Sprintf("p%d/q%d/f%d.go", i%5, i%7, i), 20+(i*37)%150))
	}
	const target = 500
	chunks, err := (&SplitStrategy{TargetTokens: target}).Chunk(diffs, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i, c := range chunks {
		for _, d := range c {
			seen[d.NewPath]++
		}
		if got := EstimateTokens(c); got > target && len(c) > 1 {
			t.Errorf("chunk %d holds %d files and ~%d tokens, over the target %d", i, len(c), got, target)
		}
	}
	if len(seen) != len(diffs) {
		t.Fatalf("%d distinct files in the chunks, want %d", len(seen), len(diffs))
	}
	for p, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times", p, n)
		}
	}
}
