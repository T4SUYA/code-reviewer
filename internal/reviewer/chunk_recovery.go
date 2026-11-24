package reviewer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/OpticDiff/code-reviewer/internal/diff"
	"github.com/OpticDiff/code-reviewer/internal/model"
)

// chunkRun collects what the chunk reviews of one run produce. A chunk whose
// model call fails, or whose answer is cut off by the output limit, is cut in
// halves and each half is reviewed on its own, down to the smallest piece
// diff.SplitChunk can make. What still cannot be reviewed is recorded as
// skipped and the rest of the run goes on.
type chunkRun struct {
	r            *Reviewer
	systemPrompt string
	// promptFor builds the user prompt for a chunk.
	promptFor func(chunk []diff.FileDiff) string

	// usage is the run's token total; it also decides whether the runtime
	// token budget lets a split go on.
	usage *model.TokenUsage

	summaries []string
	findings  []model.Finding
	// reviewed holds the paths of files whose review completed. A file cut into
	// parts is in it once the hunks of its reviewed parts add up to the file's.
	reviewed map[string]bool
	// reviewedHunks counts, per file cut into parts, the hunks of the parts
	// reviewed so far.
	reviewedHunks map[string]int
	// unreviewedPaths holds the files with a part or the whole not reviewed.
	unreviewedPaths map[string]bool
	// unreviewed lists files that were in scope and not reviewed.
	unreviewed []SkippedFile
	// truncated is set when an answer cut off by the output limit could not be
	// replaced by smaller requests.
	truncated bool
	// split counts the chunks that were cut in halves after a failure.
	split int
	// succeeded counts the model calls that returned a complete answer.
	succeeded int
	// firstErr is the first call error that was set aside.
	firstErr error
}

// settle takes the outcome of one model call for chunk and either records it
// or reviews the chunk again in parts. label names the chunk in messages. It
// returns an error only when the run cannot go on: the model is unusable or the
// run was cancelled.
func (c *chunkRun) settle(ctx context.Context, chunk []diff.FileDiff, label string, result *model.ReviewResult, err error) error {
	if err != nil {
		if ctx.Err() != nil || model.IsFatal(err) {
			return fmt.Errorf("model review (chunk %s): %w", label, err)
		}
		if c.firstErr == nil {
			c.firstErr = fmt.Errorf("model review (chunk %s): %w", label, err)
		}
		slog.Warn("chunk review failed", "chunk", label, "files", len(chunk), "error", err)
	} else {
		c.usage.Add(result.Usage)
		if !result.Truncated {
			c.succeeded++
			c.accept(chunk, result)
			return nil
		}
		slog.Warn("chunk review response was truncated by model", "chunk", label, "files", len(chunk))
	}

	if c.r.cfg.MaxTokens > 0 && c.usage.TotalTokens >= int64(c.r.cfg.MaxTokens) {
		c.skip(chunk, SkipBudget)
		return nil
	}

	parts := diff.SplitChunk(chunk)
	if len(parts) == 0 {
		c.settleFloor(chunk, result)
		return nil
	}

	c.split++
	slog.Info("reviewing the chunk again in parts", "chunk", label, "parts", len(parts))
	for i, part := range parts {
		partLabel := fmt.Sprintf("%s.%d", label, i+1)
		res, perr := c.r.provider.Review(ctx, c.systemPrompt, c.promptFor(part))
		if err := c.settle(ctx, part, partLabel, res, perr); err != nil {
			return err
		}
	}
	return nil
}

// settleFloor records a chunk that cannot be made smaller. A truncated answer
// still carries findings, which are kept; the files are reported as not fully
// reviewed.
func (c *chunkRun) settleFloor(chunk []diff.FileDiff, result *model.ReviewResult) {
	if result == nil {
		c.skip(chunk, SkipModelError)
		return
	}
	c.truncated = true
	c.summaries = append(c.summaries, result.Summary)
	c.findings = append(c.findings, result.Findings...)
	c.skip(chunk, SkipTruncated)
}

// accept records a complete answer.
func (c *chunkRun) accept(chunk []diff.FileDiff, result *model.ReviewResult) {
	for _, d := range chunk {
		if c.unreviewedPaths[d.NewPath] {
			continue
		}
		if d.Part == nil {
			c.reviewed[d.NewPath] = true
			continue
		}
		c.reviewedHunks[d.NewPath] += len(d.Hunks)
		if c.reviewedHunks[d.NewPath] >= d.Part.TotalHunks {
			c.reviewed[d.NewPath] = true
		}
	}
	c.summaries = append(c.summaries, result.Summary)
	c.findings = append(c.findings, result.Findings...)
}

// skip records the files of chunk as not reviewed. A file is listed once, with
// the reason of its first part that failed, and is never counted as reviewed.
func (c *chunkRun) skip(chunk []diff.FileDiff, reason string) {
	for _, d := range chunk {
		path := d.NewPath
		if path == "" {
			path = d.OldPath
		}
		delete(c.reviewed, d.NewPath)
		if c.unreviewedPaths[path] {
			continue
		}
		c.unreviewedPaths[path] = true
		c.unreviewed = append(c.unreviewed, SkippedFile{Path: path, Reason: reason})
	}
}

// failure returns the error that ends the run when no model call of it ever
// returned a complete answer: nothing was reviewed, so reporting every file as
// skipped would hide a setup problem behind a normal-looking review.
func (c *chunkRun) failure() error {
	if c.succeeded == 0 && c.firstErr != nil {
		return c.firstErr
	}
	return nil
}
