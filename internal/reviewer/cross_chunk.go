package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/internal/model"
	pkgmodel "github.com/OpticDiff/code-reviewer/pkg/model"
)

const (
	// crossChunkMaxFindings is the most findings listed to the cross-chunk step.
	crossChunkMaxFindings = 100
	// crossChunkPromptShare is the share of the context limit the step's prompt
	// may fill (1/10).
	crossChunkPromptShare = 10
	// crossChunkOutputReserve is the token allowance for the step's answer when
	// checking it against the remaining budget.
	crossChunkOutputReserve = 4096
	// maxChangedLineRanges is the most ranges listed per file.
	maxChangedLineRanges = 20
)

// crossChunkDigestFor bounds a chunk's digest and names its files. It returns
// false when the chunk reported nothing usable.
func crossChunkDigestFor(chunk []diff.FileDiff, result *model.ReviewResult) (model.ChunkDigest, bool) {
	if result == nil || result.Digest == nil {
		return model.ChunkDigest{}, false
	}
	d := result.Digest.Bounded()
	if d.Empty() {
		return model.ChunkDigest{}, false
	}
	seen := map[string]bool{}
	for _, f := range chunk {
		if !seen[f.NewPath] {
			seen[f.NewPath] = true
			d.Files = append(d.Files, f.NewPath)
		}
	}
	return d, true
}

// crossChunkFindings runs the cross-chunk step: one request with the digests
// of the chunks and the findings so far, which may report only findings that
// span chunks. It returns those findings, marked with their origin, and the
// request's usage. Any failure is logged and yields no findings; the step
// never fails the run. remaining is the token budget left (0 = unlimited).
func (r *Reviewer) crossChunkFindings(ctx context.Context, title string, digests []model.ChunkDigest, found []model.Finding, diffs []diff.FileDiff, tokenLimit int, remaining int64) ([]model.Finding, *model.TokenUsage) {
	if len(digests) < 2 {
		slog.Info("cross-chunk step skipped: fewer than two chunks reported a digest", "digests", len(digests))
		return nil, nil
	}

	listed := make([]model.CrossChunkFinding, 0, min(len(found), crossChunkMaxFindings))
	for _, f := range found {
		if len(listed) == crossChunkMaxFindings {
			break
		}
		listed = append(listed, model.CrossChunkFinding{File: f.File, Line: f.Line, Severity: f.Severity, Title: f.Title})
	}
	ranges := make([]string, 0, len(diffs))
	for _, d := range diffs {
		if r := changedLineRanges(d); r != "" {
			ranges = append(ranges, d.NewPath+": "+r)
		}
	}
	system := model.BuildCrossChunkSystemPrompt()
	user := model.BuildCrossChunkUserPrompt(title, digests, listed, ranges, tokenLimit/crossChunkPromptShare*4)

	estimate := int64((len(system)+len(user))/4 + crossChunkOutputReserve)
	if remaining > 0 && estimate > remaining {
		slog.Info("cross-chunk step skipped: not enough token budget left", "estimated_tokens", estimate, "remaining", remaining)
		return nil, nil
	}

	slog.Info("cross-chunk step started", "chunks", len(digests), "findings_so_far", len(found), "estimated_tokens", estimate)
	result, err := r.provider.Review(ctx, system, user)
	if err != nil {
		slog.Warn("cross-chunk step failed, keeping the chunk findings", "error", err)
		return nil, nil
	}
	if result.Truncated {
		slog.Warn("cross-chunk step response was truncated, discarding its findings")
		return nil, result.Usage
	}
	out := make([]model.Finding, len(result.Findings))
	for i, f := range result.Findings {
		f.Origin = pkgmodel.OriginCrossChunk
		out[i] = f
	}
	slog.Info("cross-chunk step finished", "findings", len(out))
	return out, result.Usage
}

// changedLineRanges lists the added lines of a file as ranges of new-file line
// numbers ("10-12, 20"), at most maxChangedLineRanges of them.
func changedLineRanges(d diff.FileDiff) string {
	var lines []int
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			if l.Type == diff.LineAdded && l.NewLineNo > 0 {
				lines = append(lines, l.NewLineNo)
			}
		}
	}
	sort.Ints(lines)
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] <= lines[j]+1 {
			j++
		}
		if lines[i] == lines[j] {
			parts = append(parts, strconv.Itoa(lines[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}
		i = j + 1
	}
	if len(parts) > maxChangedLineRanges {
		parts = append(parts[:maxChangedLineRanges], "...")
	}
	return strings.Join(parts, ", ")
}
