package worker

import (
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
)

// cardBranchPrefix is the namespace every card branch lives in; the push and
// delete guards refuse to rewrite or delete anything outside it.
const cardBranchPrefix = "cm/"

// jiraSourceSystem is the source system CM's Jira import records on a card.
const jiraSourceSystem = "jira"

// maxJiraKeyLen keeps the branch well under git's per-component and CM's
// 255-byte branch limits; real Jira Cloud keys are far shorter.
const maxJiraKeyLen = 64

// jiraKeyPattern is the uppercase issue-key shape. GitHub for Jira links a
// branch only on an uppercase key, and the shape keeps ref-unsafe characters
// out of every refspec.
var jiraKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]+-[1-9][0-9]*$`)

// cardBranchName is the one derivation of a run's work branch. It reads only
// the immutable card ID and source, so a resumed or re-triggered run lands on
// the branch an earlier run pushed.
func cardBranchName(cardID string, src cmclient.CardSource) string {
	branch := cardBranchPrefix + strings.ToLower(cardID)

	if src.System == jiraSourceSystem && len(src.ExternalID) <= maxJiraKeyLen &&
		jiraKeyPattern.MatchString(src.ExternalID) {
		branch += "-" + src.ExternalID
	}

	return branch
}
