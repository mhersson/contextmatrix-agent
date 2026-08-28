package orchestrator

import "context"

// CheckResult is one CI check as gh pr checks reports it.
type CheckResult struct {
	Name        string `json:"name"`
	Bucket      string `json:"bucket"` // pass|fail|pending|skipping|cancel
	Link        string `json:"link"`
	Description string `json:"description"`
}

// ReviewComment is one line comment from a PR review.
type ReviewComment struct {
	Path string
	Body string
}

// CopilotReview is the latest completed Copilot review on the PR.
type CopilotReview struct {
	CommitID string
	Body     string
	Comments []ReviewComment
}

// PermanentPollError is a CI poll failure that will repeat on every poll for the
// rest of the run - a gh without the flags the fallback poll needs, a token
// without the permission it needs. The CI gate parks on it at once instead of
// looping to the wait deadline; Err is the gh text verbatim, the only
// diagnostic the card can offer.
type PermanentPollError struct {
	Err string
}

func (e *PermanentPollError) Error() string {
	return "permanent poll error: " + e.Err
}

// PRGates is the gh seam for the pr_gates phase. The worker implements it on
// PRCreator; tests inject a fake.
type PRGates interface {
	Checks(ctx context.Context, prURL string) ([]CheckResult, error)
	HeadSHA(ctx context.Context, prURL string) (string, error)
	CopilotRequested(ctx context.Context, prURL string) (bool, error)

	// RequestCopilotReview requests a Copilot review and reports whether the
	// request confirmably took effect: false with a nil error means the
	// request could not be confirmed - usually the API accepted it without
	// adding the reviewer, but an unreadable response reads the same way.
	RequestCopilotReview(ctx context.Context, prURL string) (bool, error)
	CopilotReview(ctx context.Context, prURL string) (*CopilotReview, error)
	FailureLogs(ctx context.Context, prURL string, failed []CheckResult) (string, error)

	// FindPRURL returns the URL of the open PR for the workspace's current
	// branch, or "" when the branch has none. Recovery probe for a gated card
	// whose recorded PR creation failed - a PR may exist anyway.
	FindPRURL(ctx context.Context) (string, error)
}
