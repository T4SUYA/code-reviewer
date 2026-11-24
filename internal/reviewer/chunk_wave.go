package reviewer

import (
	"context"
	"sync"

	"github.com/OpticDiff/code-reviewer/internal/model"
)

// reviewWave sends one prompt per chunk to the model and returns each call's
// result and error in prompt order, whatever order the calls finish in. Exactly
// one of the two is set for every prompt.
//
// A wave of one is a plain call. Larger waves run all calls at once and wait
// for every call to finish. Rate limiting is handled by the provider's own
// retry, per call. What a failed call means for the run is decided by
// chunkRun.settle.
func (r *Reviewer) reviewWave(ctx context.Context, systemPrompt string, userPrompts []string) ([]*model.ReviewResult, []error) {
	results := make([]*model.ReviewResult, len(userPrompts))
	errs := make([]error, len(userPrompts))

	if len(userPrompts) == 1 {
		results[0], errs[0] = r.provider.Review(ctx, systemPrompt, userPrompts[0])
		return results, errs
	}

	var wg sync.WaitGroup
	for i, prompt := range userPrompts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = r.provider.Review(ctx, systemPrompt, prompt)
		}()
	}
	wg.Wait()
	return results, errs
}
