package reviewer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	// digests holds what the chunks that were asked for one reported.
	digests []model.ChunkDigest
	// wantDigest is set when the chunks were asked for a digest.
	wantDigest bool
	// recoveryCalls counts the model calls made to review chunks again in
	// parts; maxRecovery caps them.
	recoveryCalls int
	maxRecovery   int
	// consecutiveDeadEnds counts the chunks or pieces that ended unreviewed
	// since the last complete answer; tripped is set when the circuit breaker
	// stops all further model calls.
	consecutiveDeadEnds int
	tripped             bool
	// firstErr is the first call error that was set aside.
	firstErr error
}

// chunkMark is the state of a chunkRun before a chunk is settled.
type chunkMark struct {
	findings, split, unreviewed int
}

func (c *chunkRun) snapshot() chunkMark {
	return chunkMark{findings: len(c.findings), split: c.split, unreviewed: len(c.unreviewed)}
}

// logChunkFinished logs, at info level, that a chunk of the review is done:
// its position, files, how long it took (calls and any retries in parts), the
// findings it added and how it ended. A chunk that was cut in parts reports
// "split" and, when part of it could not be reviewed, "skipped_files".
func (c *chunkRun) logChunkFinished(index, of int, chunk []diff.FileDiff, before chunkMark, took time.Duration) {
	outcome := "reviewed"
	switch {
	case len(c.unreviewed) > before.unreviewed && c.split == before.split:
		outcome = "skipped"
	case c.split > before.split:
		outcome = "split"
	}
	slog.Info("chunk finished",
		"chunk", index,
		"of", of,
		"files", len(chunk),
		"duration_ms", took.Milliseconds(),
		"findings", len(c.findings)-before.findings,
		"outcome", outcome,
		"skipped_files", len(c.unreviewed)-before.unreviewed,
	)
}

const (
	// recoveryCallsBase and recoveryCallsPerChunk set how many extra model calls
	// a run may make to review failed chunks again in parts: 2 per initial chunk
	// plus 10. A chunk that fails once the allowance is spent is skipped without
	// being cut.
	recoveryCallsBase     = 10
	recoveryCallsPerChunk = 2
	// breakerDeadEnds is how many chunks or pieces in a row may end unreviewed,
	// with no complete answer in between, before the run stops calling the model.
	breakerDeadEnds = 5
)

// recoveryCallsFor returns the recovery allowance of a run with n initial chunks.
func recoveryCallsFor(n int) int { return recoveryCallsPerChunk*n + recoveryCallsBase }

// budgetSpent reports whether the runtime token budget is used up.
func (c *chunkRun) budgetSpent() bool {
	return c.r.cfg.MaxTokens > 0 && c.usage.TotalTokens >= int64(c.r.cfg.MaxTokens)
}

// deadEnd records a chunk or piece that ended unreviewed and trips the circuit
// breaker after breakerDeadEnds of them in a row. It returns an error when the
// breaker trips in a run that never had a complete answer: that run is failing
// for a reason that is not about any one file.
func (c *chunkRun) deadEnd() error {
	c.consecutiveDeadEnds++
	if c.consecutiveDeadEnds < breakerDeadEnds || c.tripped {
		return nil
	}
	c.tripped = true
	slog.Warn("circuit breaker: too many chunks in a row could not be reviewed, not calling the model again",
		"dead_ends", c.consecutiveDeadEnds, "complete_answers", c.succeeded)
	if c.succeeded == 0 && c.firstErr != nil {
		return c.firstErr
	}
	return nil
}

// settle takes the outcome of one model call for chunk and either records it
// or reviews the chunk again in parts. label names the chunk in messages. It
// returns an error only when the run cannot go on: the model is unusable, the
// run was cancelled, or the circuit breaker tripped before any call succeeded.
//
// A chunk is cut in parts only when a smaller request can help: the answer was
// truncated or unusable, or the request was too large. Any other error that
// outlived the provider's retries (rate limit, quota, server or network error,
// timeout) skips the chunk, because the parts would meet the same error.
func (c *chunkRun) settle(ctx context.Context, chunk []diff.FileDiff, label string, result *model.ReviewResult, err error) error {
	splittable := false
	if err != nil {
		c.usage.Add(model.UsageOf(err))
		if ctx.Err() != nil || model.IsFatal(err) {
			return fmt.Errorf("model review (chunk %s): %w", label, err)
		}
		if c.firstErr == nil {
			c.firstErr = fmt.Errorf("model review (chunk %s): %w", label, err)
		}
		splittable = model.IsSplittable(err)
		slog.Warn("chunk review failed", "chunk", label, "files", len(chunk), "splittable", splittable, "error", err)
	} else {
		c.usage.Add(result.Usage)
		if !result.Truncated {
			c.succeeded++
			c.consecutiveDeadEnds = 0
			c.accept(chunk, result)
			return nil
		}
		splittable = true
		slog.Warn("chunk review response was truncated by model", "chunk", label, "files", len(chunk))
	}

	if c.budgetSpent() {
		c.keepPartial(result)
		c.skip(chunk, SkipBudget)
		return nil
	}
	if !splittable || c.tripped {
		c.skip(chunk, SkipModelError)
		return c.deadEnd()
	}

	parts := diff.SplitChunk(chunk)
	if len(parts) == 0 {
		c.settleFloor(chunk, result)
		return c.deadEnd()
	}
	if c.recoveryCalls+len(parts) > c.maxRecovery {
		slog.Warn("recovery allowance spent, not cutting the chunk again", "chunk", label, "allowance", c.maxRecovery)
		c.settleFloor(chunk, result)
		return c.deadEnd()
	}

	c.recoveryCalls += len(parts) // Reserved now, so nested cuts cannot overrun the allowance.
	c.split++
	slog.Info("reviewing the chunk again in parts", "chunk", label, "parts", len(parts))
	for i, part := range parts {
		partLabel := fmt.Sprintf("%s.%d", label, i+1)
		switch {
		case c.budgetSpent():
			c.skip(part, SkipBudget)
			continue
		case c.tripped:
			c.skip(part, SkipModelError)
			continue
		}
		res, perr := c.r.provider.Review(ctx, c.systemPrompt, c.promptFor(part))
		if err := c.settle(ctx, part, partLabel, res, perr); err != nil {
			return err
		}
	}
	return nil
}

// keepPartial keeps the findings of an answer that was cut off, for a chunk
// that will not be reviewed again.
func (c *chunkRun) keepPartial(result *model.ReviewResult) {
	if result == nil {
		return
	}
	c.truncated = true
	c.summaries = append(c.summaries, result.Summary)
	c.findings = append(c.findings, result.Findings...)
}

// settleFloor records a chunk that cannot be made smaller. A truncated answer
// still carries findings, which are kept; the files are reported as not fully
// reviewed.
func (c *chunkRun) settleFloor(chunk []diff.FileDiff, result *model.ReviewResult) {
	if result == nil {
		c.skip(chunk, SkipModelError)
		return
	}
	c.keepPartial(result)
	c.skip(chunk, SkipTruncated)
}

// diffPath is the path a file of the change is known by: its new path, or its
// old path for a deleted file.
func diffPath(d diff.FileDiff) string {
	if d.NewPath != "" {
		return d.NewPath
	}
	return d.OldPath
}

// accept records a complete answer.
func (c *chunkRun) accept(chunk []diff.FileDiff, result *model.ReviewResult) {
	for _, d := range chunk {
		path := diffPath(d)
		if c.unreviewedPaths[path] {
			continue
		}
		if d.Part == nil {
			c.reviewed[path] = true
			continue
		}
		c.reviewedHunks[path] += len(d.Hunks)
		if c.reviewedHunks[path] >= d.Part.TotalHunks {
			c.reviewed[path] = true
		}
	}
	c.summaries = append(c.summaries, result.Summary)
	c.findings = append(c.findings, result.Findings...)
	if c.wantDigest {
		if d, ok := crossChunkDigestFor(chunk, result); ok {
			c.digests = append(c.digests, d)
		}
	}
}

// skip records the files of chunk as not reviewed. A file is listed once, with
// the reason of its first part that failed, and is never counted as reviewed.
func (c *chunkRun) skip(chunk []diff.FileDiff, reason string) {
	for _, d := range chunk {
		path := diffPath(d)
		delete(c.reviewed, path)
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
