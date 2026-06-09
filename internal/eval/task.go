package eval

import (
	"context"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// Task is one scored unit in the eval matrix. Coder and Reviewer implement it.
type Task interface {
	Name() string
	Role() registry.Role
	// Provision writes the task's fixture into an empty scratch dir.
	Provision(dir string) error
	// Prompt is the instruction handed to harness.Run.
	Prompt() string
	// Check adjudicates success from the post-run workspace and/or the model's
	// Result. Coder tasks run the hidden test against dir; reviewer tasks parse
	// res.Output for the structured verdict.
	Check(ctx context.Context, dir string, res harness.Result) (harness.Verdict, error)
}
