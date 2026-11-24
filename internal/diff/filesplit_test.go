package diff

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// hunkedDiff returns a file of n hunks, each of one added line of about
// `tokens` estimated tokens. Hunk i starts at line 100*i+1 of the new file.
func hunkedDiff(path string, n, tokens int) FileDiff {
	d := FileDiff{OldPath: path, NewPath: path}
	for i := range n {
		start := 100*i + 1
		d.Hunks = append(d.Hunks, Hunk{
			Header:   fmt.Sprintf("@@ -%d,1 +%d,2 @@", start, start),
			OldStart: start, OldCount: 1, NewStart: start, NewCount: 2,
			Lines: []DiffLine{
				{Type: LineContext, Content: "ctx", OldLineNo: start, NewLineNo: start},
				{Type: LineAdded, Content: strings.Repeat("x", tokens*4), NewLineNo: start + 1},
			},
		})
	}
	return d
}

func hunkStarts(d FileDiff) []int {
	var out []int
	for _, h := range d.Hunks {
		out = append(out, h.NewStart)
	}
	return out
}

func TestSplitFile_PartsFitTheTargetAndKeepLineNumbers(t *testing.T) {
	d := hunkedDiff("big.go", 6, 100)
	parts := SplitFile(d, 250)
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3 (two hunks of ~100 tokens each)", len(parts))
	}
	var starts []int
	for i, p := range parts {
		if p.NewPath != "big.go" || p.OldPath != "big.go" {
			t.Errorf("part %d lost the file header: %q -> %q", i+1, p.OldPath, p.NewPath)
		}
		if got := EstimateTokens([]FileDiff{p}); got > 250 {
			t.Errorf("part %d is ~%d tokens, over the target", i+1, got)
		}
		if p.Part == nil || p.Part.Label() != fmt.Sprintf("part %d of 3", i+1) {
			t.Errorf("part %d is labelled %v", i+1, p.Part)
		}
		if p.Part.TotalHunks != 6 {
			t.Errorf("part %d: TotalHunks = %d, want 6", i+1, p.Part.TotalHunks)
		}
		starts = append(starts, hunkStarts(p)...)
		for _, h := range p.Hunks {
			for _, l := range h.Lines {
				if l.Type == LineAdded && l.NewLineNo != h.NewStart+1 {
					t.Errorf("part %d: added line renumbered to %d, hunk starts at %d", i+1, l.NewLineNo, h.NewStart)
				}
			}
		}
	}
	if want := []int{1, 101, 201, 301, 401, 501}; !reflect.DeepEqual(starts, want) {
		t.Errorf("hunks of the parts = %v, want every hunk once in order %v", starts, want)
	}
	// The input is not modified.
	if len(d.Hunks) != 6 || d.Part != nil {
		t.Error("SplitFile changed its input")
	}
}

func TestSplitFile_Cases(t *testing.T) {
	tests := []struct {
		name      string
		d         FileDiff
		target    int
		wantParts int
	}{
		{"fits the target", hunkedDiff("a.go", 3, 50), 1000, 0},
		{"one hunk larger than the target is not cut", hunkedDiff("a.go", 1, 500), 100, 0},
		{"oversized hunk goes alone", hunkedDiff("a.go", 3, 500), 100, 3},
		{"no hunks", FileDiff{NewPath: "a.go"}, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := SplitFile(tt.d, tt.target)
			if len(parts) != tt.wantParts {
				t.Fatalf("got %d parts, want %d", len(parts), tt.wantParts)
			}
		})
	}
}

func TestSplitFile_MixedHunkSizes(t *testing.T) {
	d := hunkedDiff("a.go", 4, 40)
	d.Hunks[1].Lines[1].Content = strings.Repeat("x", 4*400) // One hunk of ~400 tokens.
	parts := SplitFile(d, 150)
	var got [][]int
	for _, p := range parts {
		got = append(got, hunkStarts(p))
	}
	want := [][]int{{1}, {101}, {201, 301}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parts = %v, want %v (the large hunk alone, the rest packed in order)", got, want)
	}
}

func TestSplitChunk_SingleFileSplitsByHunks(t *testing.T) {
	d := hunkedDiff("a.go", 4, 100)
	for i := range 2 { // The first two hunks outweigh the last two, so the cut is not a tie.
		d.Hunks[i].Lines[1].Content = strings.Repeat("x", 4*110)
	}
	parts := SplitChunk([]FileDiff{d})
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if got := hunkStarts(parts[0][0]); !reflect.DeepEqual(got, []int{1, 101}) {
		t.Errorf("first half hunks = %v", got)
	}
	if got := hunkStarts(parts[1][0]); !reflect.DeepEqual(got, []int{201, 301}) {
		t.Errorf("second half hunks = %v", got)
	}
	if l := parts[0][0].Part.Label(); l != "part 1 of 2" {
		t.Errorf("label = %q", l)
	}

	// A part that fails is halved again and numbered under its own number.
	sub := SplitChunk([]FileDiff{parts[1][0]})
	if len(sub) != 2 || sub[0][0].Part.Label() != "part 2.1 of 2" || sub[1][0].Part.Label() != "part 2.2 of 2" {
		t.Fatalf("sub-parts = %v", sub)
	}
	if got := hunkStarts(sub[1][0]); !reflect.DeepEqual(got, []int{301}) {
		t.Errorf("second sub-part hunks = %v", got)
	}
	// One hunk is the floor.
	if SplitChunk(sub[0]) != nil {
		t.Error("a single hunk was split")
	}
	if SplitChunk([]FileDiff{hunkedDiff("a.go", 1, 10)}) != nil {
		t.Error("a single-hunk file was split")
	}
}

func TestSplitStrategy_OversizedFileIsSplitByHunks(t *testing.T) {
	big := hunkedDiff("pkg/big.go", 6, 100)
	other := sizedDiff("pkg/small.go", 50)
	chunks, err := (&SplitStrategy{TargetTokens: 250}).Chunk([]FileDiff{big, other}, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	var bigParts int
	var starts []int
	for _, c := range chunks {
		if got := EstimateTokens(c); got > 250 {
			t.Errorf("chunk of ~%d tokens is over the target", got)
		}
		for _, d := range c {
			if d.NewPath == "pkg/big.go" {
				bigParts++
				starts = append(starts, hunkStarts(d)...)
			}
		}
	}
	if bigParts != 3 {
		t.Errorf("big.go came in %d parts, want 3", bigParts)
	}
	if want := []int{1, 101, 201, 301, 401, 501}; !reflect.DeepEqual(starts, want) {
		t.Errorf("hunks over the chunks = %v, want each once in order", starts)
	}
	// A file that fits is never cut.
	chunks, err = (&SplitStrategy{TargetTokens: 5000}).Chunk([]FileDiff{big, other}, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || len(chunks[0]) != 2 || chunks[0][0].Part != nil {
		t.Errorf("chunks = %v, want one chunk of whole files", chunkPaths(chunks))
	}
}
