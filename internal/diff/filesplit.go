package diff

import (
	"strconv"
)

// FilePart says which part of a file's hunks a FileDiff carries.
type FilePart struct {
	// Number is the part's number among the parts of the file: "2", or "2.1"
	// for the first half of part 2 after that part had to be cut again.
	Number string
	// Of is the number of parts the file was first cut into.
	Of int
	// TotalHunks is the number of hunks of the whole file, so the parts of a
	// file can be checked to cover it.
	TotalHunks int
}

// Label renders the part for a prompt or a message, e.g. "part 2 of 3".
func (p FilePart) Label() string {
	return "part " + p.Number + " of " + strconv.Itoa(p.Of)
}

// SplitFile cuts a file larger than target estimated tokens into parts at hunk
// boundaries. Each part carries the file's paths, a Part marker, and whole
// hunks with their original line numbers; hunks are added to a part in order
// until the next one would take it over target. A hunk larger than target is
// a part of its own and is not cut. SplitFile returns nil when the file fits
// target or has fewer than two hunks. The input is not changed.
func SplitFile(d FileDiff, target int) []FileDiff {
	if len(d.Hunks) < 2 || estimateFileDiffTokens(&d) <= target {
		return nil
	}
	header := len(d.OldPath) + len(d.NewPath)
	var groups [][]Hunk
	var current []Hunk
	chars := header
	for _, h := range d.Hunks {
		size := hunkChars(&h)
		if len(current) > 0 && (chars+size)/4 > target {
			groups = append(groups, current)
			current, chars = nil, header
		}
		current = append(current, h)
		chars += size
	}
	groups = append(groups, current)
	if len(groups) < 2 {
		return nil
	}

	parts := make([]FileDiff, len(groups))
	for i, hunks := range groups {
		parts[i] = d
		parts[i].Hunks = append([]Hunk(nil), hunks...)
		parts[i].Part = &FilePart{Number: strconv.Itoa(i + 1), Of: len(groups), TotalHunks: len(d.Hunks)}
	}
	return parts
}

// splitFileInHalf cuts a file at a hunk boundary into two parts of about equal
// estimated size. A part that is cut again is numbered under its own number
// ("2.1" and "2.2" of part 2). It returns nil for a file of fewer than two
// hunks.
func splitFileInHalf(d FileDiff) []FileDiff {
	if len(d.Hunks) < 2 {
		return nil
	}
	total := 0
	for i := range d.Hunks {
		total += hunkChars(&d.Hunks[i])
	}
	cut, running := 1, 0
	for i := range d.Hunks[:len(d.Hunks)-1] {
		running += hunkChars(&d.Hunks[i])
		cut = i + 1
		if running*2 >= total {
			break
		}
	}

	part := FilePart{Number: "", Of: 2, TotalHunks: len(d.Hunks)}
	prefix := ""
	if d.Part != nil {
		part = *d.Part
		prefix = part.Number + "."
	}
	halves := make([]FileDiff, 2)
	for i, hunks := range [][]Hunk{d.Hunks[:cut], d.Hunks[cut:]} {
		halves[i] = d
		halves[i].Hunks = append([]Hunk(nil), hunks...)
		p := part
		p.Number = prefix + strconv.Itoa(i+1)
		halves[i].Part = &p
	}
	return halves
}
