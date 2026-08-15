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

// PRGates is the gh seam for the pr_gates phase. The worker implements it on
// PRCreator; tests inject a fake.
type PRGates interface {
	Checks(ctx context.Context, prURL string) ([]CheckResult, error)
	HeadSHA(ctx context.Context, prURL string) (string, error)
	CopilotRequested(ctx context.Context, prURL string) (bool, error)
	RequestCopilotReview(ctx context.Context, prURL string) error
	CopilotReview(ctx context.Context, prURL string) (*CopilotReview, error)
	FailureLogs(ctx context.Context, prURL string, failed []CheckResult) (string, error)
}
