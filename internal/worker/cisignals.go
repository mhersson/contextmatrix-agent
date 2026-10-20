package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"time"
)

// The CI-config table is the one place the worker names CI systems: a repo that
// carries none of these and shows no CI activity is treated as having no CI.
var (
	ciConfigRootFiles = []string{"Jenkinsfile", ".travis.yml", "azure-pipelines.yml", ".drone.yml", ".woodpecker.yml"}
	ciConfigRootDirs  = []string{".buildkite", ".woodpecker"}
)

const (
	ciCircleDir      = ".circleci"
	ciCircleFile     = "config.yml"
	ciGitHubDir      = ".github"
	ciWorkflowsDir   = "workflows"
	mergedPullsLimit = "30"
)

// treeArgs builds the gh api invocation that reads one git tree by SHA.
func treeArgs(owner, repo, sha string) []string {
	return []string{"api", fmt.Sprintf("repos/%s/%s/git/trees/%s", owner, repo, sha)}
}

// baseRefArgs builds the gh pr view invocation that reports the PR's base
// branch name and head commit.
func baseRefArgs(prURL string) []string {
	return []string{"pr", "view", prURL, "--json", "baseRefName,baseRefOid"}
}

// checkRunsArgs builds the gh api invocation that counts a commit's check runs.
func checkRunsArgs(owner, repo, sha string) []string {
	return []string{"api", fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=1", owner, repo, sha)}
}

// mergedPullsArgs lists recently updated closed PRs into base. The base rides
// as a field because a branch name may contain slashes.
func mergedPullsArgs(owner, repo, base string) []string {
	return []string{
		"api", "--method", "GET", "-f", "state=closed", "-f", "base=" + base,
		"-f", "sort=updated", "-f", "direction=desc", "-f", "per_page=" + mergedPullsLimit,
		fmt.Sprintf("repos/%s/%s/pulls", owner, repo),
	}
}

type gitTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// readTree reads one git tree. A truncated listing is an error: absence of a
// CI file cannot be concluded from a partial tree.
func (p *PRCreator) readTree(ctx context.Context, owner, repo, sha string) ([]gitTreeEntry, error) {
	out, err := p.runGH(ctx, "", false, treeArgs(owner, repo, sha)...)
	if err != nil {
		return nil, err
	}

	var doc struct {
		Tree      []gitTreeEntry `json:"tree"`
		Truncated bool           `json:"truncated"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse tree %s: %w", sha, err)
	}

	if doc.Truncated {
		return nil, fmt.Errorf("tree %s: listing truncated", sha)
	}

	return doc.Tree, nil
}

func findEntry(entries []gitTreeEntry, name, typ string) (gitTreeEntry, bool) {
	for _, e := range entries {
		if e.Path == name && e.Type == typ {
			return e, true
		}
	}

	return gitTreeEntry{}, false
}

// CIConfigFiles lists the known CI config present in the PR head tree. A
// directory entry is reported with a trailing slash.
func (p *PRCreator) CIConfigFiles(ctx context.Context, prURL string) ([]string, error) {
	files, err := p.ciConfigFiles(ctx, prURL)
	if err != nil {
		return nil, fmt.Errorf("gh api ci config: %w", err)
	}

	return files, nil
}

func (p *PRCreator) ciConfigFiles(ctx context.Context, prURL string) ([]string, error) {
	owner, repo, _, err := parsePRPath(prURL)
	if err != nil {
		return nil, err
	}

	sha, err := p.HeadSHA(ctx, prURL)
	if err != nil {
		return nil, err
	}

	root, err := p.readTree(ctx, owner, repo, sha)
	if err != nil {
		return nil, err
	}

	var found []string

	for _, name := range ciConfigRootFiles {
		if _, ok := findEntry(root, name, "blob"); ok {
			found = append(found, name)
		}
	}

	for _, name := range ciConfigRootDirs {
		if _, ok := findEntry(root, name, "tree"); ok {
			found = append(found, name+"/")
		}
	}

	if dir, ok := findEntry(root, ciCircleDir, "tree"); ok {
		entries, err := p.readTree(ctx, owner, repo, dir.SHA)
		if err != nil {
			return nil, err
		}

		if _, ok := findEntry(entries, ciCircleFile, "blob"); ok {
			found = append(found, ciCircleDir+"/"+ciCircleFile)
		}
	}

	workflows, err := p.workflowFiles(ctx, owner, repo, root)
	if err != nil {
		return nil, err
	}

	return append(found, workflows...), nil
}

// workflowFiles returns the .github/workflows/*.yml|*.yaml files of root.
func (p *PRCreator) workflowFiles(ctx context.Context, owner, repo string, root []gitTreeEntry) ([]string, error) {
	github, ok := findEntry(root, ciGitHubDir, "tree")
	if !ok {
		return nil, nil
	}

	entries, err := p.readTree(ctx, owner, repo, github.SHA)
	if err != nil {
		return nil, err
	}

	dir, ok := findEntry(entries, ciWorkflowsDir, "tree")
	if !ok {
		return nil, nil
	}

	files, err := p.readTree(ctx, owner, repo, dir.SHA)
	if err != nil {
		return nil, err
	}

	var found []string

	for _, e := range files {
		if ext := path.Ext(e.Path); e.Type == "blob" && (ext == ".yml" || ext == ".yaml") {
			found = append(found, path.Join(ciGitHubDir, ciWorkflowsDir, e.Path))
		}
	}

	return found, nil
}

// BaseCIActivity reports the PR's base branch name and whether its head commit
// carries any check run or commit status.
func (p *PRCreator) BaseCIActivity(ctx context.Context, prURL string) (string, bool, error) {
	owner, repo, _, err := parsePRPath(prURL)
	if err != nil {
		return "", false, fmt.Errorf("gh api base ci activity: %w", err)
	}

	base, baseSHA, err := p.baseRef(ctx, prURL)
	if err != nil {
		return "", false, fmt.Errorf("gh api base ci activity: %w", err)
	}

	active, err := p.commitCIActivity(ctx, owner, repo, baseSHA)
	if err != nil {
		return "", false, fmt.Errorf("gh api base ci activity: %w", err)
	}

	return base, active, nil
}

func (p *PRCreator) baseRef(ctx context.Context, prURL string) (name, sha string, err error) {
	out, err := p.runGH(ctx, "", false, baseRefArgs(prURL)...)
	if err != nil {
		return "", "", err
	}

	var doc struct {
		BaseRefName string `json:"baseRefName"`
		BaseRefOid  string `json:"baseRefOid"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return "", "", fmt.Errorf("parse base ref: %w", err)
	}

	return doc.BaseRefName, doc.BaseRefOid, nil
}

// LastMergedCIActivity reports whether the newest PR merged into the same base
// has any check run or commit status on its head SHA; false when none merged.
func (p *PRCreator) LastMergedCIActivity(ctx context.Context, prURL string) (bool, error) {
	active, err := p.lastMergedCIActivity(ctx, prURL)
	if err != nil {
		return false, fmt.Errorf("gh api last merged ci activity: %w", err)
	}

	return active, nil
}

func (p *PRCreator) lastMergedCIActivity(ctx context.Context, prURL string) (bool, error) {
	owner, repo, _, err := parsePRPath(prURL)
	if err != nil {
		return false, err
	}

	base, _, err := p.baseRef(ctx, prURL)
	if err != nil {
		return false, err
	}

	out, err := p.runGH(ctx, "", false, mergedPullsArgs(owner, repo, base)...)
	if err != nil {
		return false, err
	}

	var pulls []struct {
		MergedAt *time.Time `json:"merged_at"`
		Head     struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}

	if err := json.Unmarshal([]byte(out), &pulls); err != nil {
		return false, fmt.Errorf("parse pulls: %w", err)
	}

	newest := -1

	for i, pr := range pulls {
		if pr.MergedAt != nil && (newest < 0 || pr.MergedAt.After(*pulls[newest].MergedAt)) {
			newest = i
		}
	}

	if newest < 0 {
		return false, nil
	}

	return p.commitCIActivity(ctx, owner, repo, pulls[newest].Head.SHA)
}

// commitCIActivity reports whether sha has any check run, Actions run or commit
// status, stopping at the first source that has one. It never arms
// checksFallback: a token refused the Checks API reads Actions runs for this
// probe only.
func (p *PRCreator) commitCIActivity(ctx context.Context, owner, repo, sha string) (bool, error) {
	active, err := p.checkRunsActive(ctx, owner, repo, sha)
	if err != nil || active {
		return active, err
	}

	out, err := p.runGH(ctx, "", false, combinedStatusArgs(owner, repo, sha)...)
	if err != nil {
		return false, fmt.Errorf("commit status: %w", err)
	}

	statuses, err := parseCombinedStatus(out)
	if err != nil {
		return false, err
	}

	return len(statuses) > 0, nil
}

// checkRunsActive asks the Checks API, or the Actions runs when the token is
// refused it or the sticky fallback is already armed.
func (p *PRCreator) checkRunsActive(ctx context.Context, owner, repo, sha string) (bool, error) {
	if !p.checksFallback {
		out, err := p.runGH(ctx, "", false, checkRunsArgs(owner, repo, sha)...)
		if err == nil {
			var doc struct {
				TotalCount int `json:"total_count"`
			}

			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				return false, fmt.Errorf("parse check runs: %w", err)
			}

			return doc.TotalCount > 0, nil
		}

		if !checksInaccessiblePattern.MatchString(err.Error()) {
			return false, fmt.Errorf("check runs: %w", err)
		}
	}

	out, err := p.runGH(ctx, "", false, runListArgs(owner, repo, sha)...)
	if err != nil {
		return false, fmt.Errorf("run list: %w", err)
	}

	runs, err := parseRunList(out)
	if err != nil {
		return false, err
	}

	return len(runs) > 0, nil
}
