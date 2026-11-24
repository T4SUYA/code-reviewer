package reviewer

import (
	"context"
	"sync"
	"time"

	"github.com/OpticDiff/code-reviewer/internal/model"
)

// reviewWave sends one prompt per chunk to the model and returns each call's
// result and error in prompt order, whatever order the calls finish in. Exactly
// one of the two is set for every prompt, and how long each call took.
//
// A wave of one is a plain call. Larger waves run all calls at once and wait
// for every call to finish. Rate limiting is handled by the provider's own
// retry, per call. What a failed call means for the run is decided by
// chunkRun.settle.
func (r *Reviewer) reviewWave(ctx context.Context, systemPrompt string, userPrompts []string) ([]*model.ReviewResult, []error, []time.Duration) {
	results := make([]*model.ReviewResult, len(userPrompts))
	errs := make([]error, len(userPrompts))
	took := make([]time.Duration, len(userPrompts))
	call := func(i int) {
		start := time.Now()
		results[i], errs[i] = r.provider.Review(ctx, systemPrompt, userPrompts[i])
		took[i] = time.Since(start)
	}

	if len(userPrompts) == 1 {
		call(0)
		return results, errs, took
	}

	var wg sync.WaitGroup
	for i := range userPrompts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call(i)
		}()
	}
	wg.Wait()
	return results, errs, took
}
