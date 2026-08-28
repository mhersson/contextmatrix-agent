package orchestrator

import "context"

// CheckResult is one CI check as gh pr checks reports it.
type CheckResult struct {
	Name        string `json:"name"`
	Bucket      string `json:"bucket"` // pass|fail|pending|skipping|cancel
	Link        string `json:"link"`
	Description string `json:"description"`
}

// ReviewComment is one line comment from a PR review. ID is the REST comment
// id the reply endpoint is addressed by; zero on a value that never carried
// one.
type ReviewComment struct {
	ID   int64
	Path string
	Body string
}

// ReviewThread is one review-comment thread on the PR, as GraphQL sees it.
// ThreadID is the opaque node ID resolveReviewThread needs. CommentIDs are
// the REST databaseIds of the thread's comments in order - the first is the
// root comment a reply targets. RootPath and RootBody are the root comment's
// location and text, the inputs of the dedupe key that matches threads to
// triaged card lines across a park/resume. ReplyCount is the number of
// comments beyond the root: a thread that already has any reply is one the
// gate must not reply to again.
type ReviewThread struct {
	ThreadID   string
	IsResolved bool
	CommentIDs []int64
	ReplyCount int
	RootPath   string
	RootBody   string
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

	// ReviewThreads lists the PR's review-comment threads; ReplyToReviewComment
	// and ResolveReviewThread write the gate's triage verdicts back into them.
	// All three serve a best-effort path: their errors are logged on the card,
	// never escalated.
	ReviewThreads(ctx context.Context, prURL string) ([]ReviewThread, error)
	ReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error
	ResolveReviewThread(ctx context.Context, threadID string) error

	FailureLogs(ctx context.Context, prURL string, failed []CheckResult) (string, error)

	// FindPRURL returns the URL of the open PR for the workspace's current
	// branch, or "" when the branch has none. Recovery probe for a gated card
	// whose recorded PR creation failed - a PR may exist anyway.
	FindPRURL(ctx context.Context) (string, error)
}
