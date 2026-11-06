package reviewer

import (
	"fmt"

	"github.com/OpticDiff/code-reviewer/internal/deptools"
)

// registerDependencyTools registers the dependency tools named in the agent
// config. With none named it registers nothing, so the prompt and response
// schema stay those of the built-in tools.
func registerDependencyTools(reg *ToolRegistry, cfg AgentConfig) error {
	if len(cfg.DependencyTools) == 0 {
		return nil
	}
	opts := cfg.DependencyToolOptions
	opts.RepoRoot = cfg.RepoRoot
	specs, err := deptools.Build(cfg.DependencyTools, opts)
	if err != nil {
		return fmt.Errorf("agent tools: %w", err)
	}
	for _, s := range specs {
		err := reg.Register(Tool{
			Name:             s.Name,
			Description:      s.Description,
			Usage:            s.Usage,
			InputSchema:      s.InputSchema,
			Timeout:          s.Timeout,
			UntrustedOutput:  s.Untrusted,
			ExternalEvidence: s.ExternalEvidence,
			Inconclusive:     s.Inconclusive,
			Execute:          s.Execute,
		})
		if err != nil {
			return fmt.Errorf("registering agent tool: %w", err)
		}
	}
	return nil
}
