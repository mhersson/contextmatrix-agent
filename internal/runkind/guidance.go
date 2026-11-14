package runkind

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// prHost is the pull request side of guidance_pr; *worker.PRCreator satisfies it.
type prHost interface {
	OpenPRForHead(ctx context.Context, head string) (string, string, error)
	Create(ctx context.Context, title, body, base, head string) (string, error)
	EditPR(ctx context.Context, prURL, title, body string) error
}

// guidanceDeps are the guidance_pr kind's effects besides git: publish masks
// everything the run writes.
type guidanceDeps struct {
	prs        prHost
	candidates func(ws string) []orchestrator.VerifyCandidate
	absent     func(ctx context.Context, ws string, tokens, declared []string) ([]string, error)
	publish    func(string) string
}

func newGuidanceDeps(spec Spec, ws string) guidanceDeps {
	return guidanceDeps{
		prs:        worker.NewPRCreator(ws, spec.SecretsPath, spec.CACertFile, spec.RepoURL),
		candidates: orchestrator.VerifyCandidates,
		absent:     absentFromRepo,
		publish:    operatorMask(spec),
	}
}

// stale reports whether the branch or its PR is not as the caller expects.
func stale(expectHead, remoteTip string, openPR bool, openHead string) bool {
	if expectHead == "" {
		return openPR
	}

	return !openPR || openHead != expectHead || remoteTip != expectHead
}

// notAllowed returns the absent entries not in allowed, compared exactly.
func notAllowed(absent, allowed []string) []string {
	var out []string

	for _, a := range absent {
		if !slices.Contains(allowed, a) {
			out = append(out, a)
		}
	}

	return out
}

// guidanceRun is one guidance_pr run: its git, its masked input and its branches.
type guidanceRun struct {
	git          *worker.Git
	d            guidanceDeps
	ws           string
	in           protocol.GuidancePRInput
	branch, base string
}

// guidancePR appends in.Section to the project's block in the instruction file,
// commits that one file on the project's guidance branch, pushes it and opens
// or updates the PR. It never merges, closes or labels a PR.
func guidancePR(ctx context.Context, ws string, spec Spec, in protocol.GuidancePRInput, d guidanceDeps) (protocol.GuidancePRResult, error) {
	in.Section, in.Title, in.Body = d.publish(in.Section), d.publish(in.Title), d.publish(in.Body)

	git := worker.NewGit(ws, spec.SecretsPath, worker.HostFromRepoURL(spec.RepoURL), spec.CACertFile)
	remoteDefault := git.RemoteDefaultBranch(ctx)

	r := &guidanceRun{git: git, d: d, ws: ws, in: in, branch: guidanceBranch(spec.Project), base: spec.BaseBranch}
	if r.base == "" {
		r.base = remoteDefault
	}

	git.SetBranchPolicy(r.branch, r.base, remoteDefault)

	// The tip is read before the PR: a push in between makes the lease or the
	// plain push fail, which reads as stale.
	tip, err := git.RemoteTip(ctx, r.branch)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	url, head, err := d.prs.OpenPRForHead(ctx, r.branch)
	if err != nil {
		return protocol.GuidancePRResult{}, fmt.Errorf("find open PR: %w", err)
	}

	if stale(in.ExpectHead, tip, url != "", head) {
		return protocol.GuidancePRResult{Outcome: protocol.GuidanceStale}, nil
	}

	missing, err := r.textCheck(ctx)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	if len(missing) > 0 {
		return protocol.GuidancePRResult{Outcome: protocol.GuidanceRefused, AbsentFromRepo: missing}, nil
	}

	file, content, err := r.content(spec.Project)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	if err := r.startBranch(ctx, file); err != nil {
		return protocol.GuidancePRResult{}, err
	}

	committed, err := r.commit(ctx, file, content)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	if !committed {
		if in.ExpectHead == "" {
			return protocol.GuidancePRResult{}, errors.New("guidance changes nothing on the base branch")
		}

		if err := d.prs.EditPR(ctx, url, in.Title, in.Body); err != nil {
			return protocol.GuidancePRResult{}, fmt.Errorf("edit PR: %w", err)
		}

		return protocol.GuidancePRResult{Outcome: protocol.GuidanceUnchanged, PRURL: url, HeadSHA: in.ExpectHead, File: file}, nil
	}

	if lost, err := r.push(ctx, tip); err != nil {
		return protocol.GuidancePRResult{}, err
	} else if lost {
		return protocol.GuidancePRResult{Outcome: protocol.GuidanceStale}, nil
	}

	url, err = r.openPR(ctx, url)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	pushed, err := git.Head(ctx)
	if err != nil {
		return protocol.GuidancePRResult{}, err
	}

	return protocol.GuidancePRResult{Outcome: protocol.GuidancePushed, PRURL: url, HeadSHA: pushed, File: file}, nil
}

// textCheck returns the section's commands, URLs and hostnames the base branch
// lacks and the input does not allow.
func (r *guidanceRun) textCheck(ctx context.Context) ([]string, error) {
	var declared []string
	for _, c := range r.d.candidates(r.ws) {
		declared = append(declared, c.Display)
	}

	absent, err := r.d.absent(ctx, r.ws, textTokens(r.in.Section), declared)
	if err != nil {
		return nil, fmt.Errorf("text check: %w", err)
	}

	return notAllowed(absent, r.in.Allowed), nil
}

// content returns the instruction file's path and its new text: the base
// branch's file with the section appended to the project's block.
func (r *guidanceRun) content(project string) (file, text string, err error) {
	file, err = instructionFile(r.ws)
	if err != nil {
		return "", "", err
	}

	old, err := os.ReadFile(filepath.Join(r.ws, filepath.FromSlash(file)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read %s: %w", file, err)
	}

	text, err = withGuidance(string(old), project, r.in.Section)
	if err != nil {
		return "", "", err
	}

	return file, text, nil
}

// startBranch cuts the guidance branch: from the expected head, or from the
// base branch when no PR is open. file was resolved on the base branch, so the
// expected head must resolve to the same file before anything is written.
func (r *guidanceRun) startBranch(ctx context.Context, file string) error {
	if r.in.ExpectHead != "" {
		if err := r.git.Fetch(ctx, "+refs/heads/"+r.branch+":refs/remotes/origin/"+r.branch); err != nil {
			return fmt.Errorf("fetch guidance branch: %w", err)
		}

		if err := r.git.Checkout(ctx, r.in.ExpectHead); err != nil {
			return fmt.Errorf("check out expected head: %w", err)
		}

		if onBranch, err := instructionFile(r.ws); err != nil || onBranch != file {
			return errors.Join(fmt.Errorf("the instruction file on the expected head is not %s", file), err)
		}
	}

	if err := r.git.CreateBranch(ctx, r.branch); err != nil {
		return fmt.Errorf("create guidance branch: %w", err)
	}

	return nil
}

func (r *guidanceRun) commit(ctx context.Context, file, text string) (bool, error) {
	//nolint:gosec // G306: a tracked repo file, not a secret
	if err := os.WriteFile(filepath.Join(r.ws, filepath.FromSlash(file)), []byte(text), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", file, err)
	}

	return r.git.CommitPath(ctx, file, r.in.Title)
}

// push publishes the commit. With an expected head it is a plain push on top
// of it; otherwise a plain push creates the branch or a lease replaces the tip
// read at the start. A failed push after the branch moved is stale.
func (r *guidanceRun) push(ctx context.Context, tip string) (lost bool, err error) {
	expected := tip

	var pushErr error

	switch {
	case r.in.ExpectHead != "":
		expected, pushErr = r.in.ExpectHead, r.git.Push(ctx, r.branch)
	case tip == "":
		pushErr = r.git.Push(ctx, r.branch)
	default:
		pushErr = r.git.ForcePushWithLease(ctx, r.branch, tip)
	}

	if pushErr == nil {
		return false, nil
	}

	if now, tipErr := r.git.RemoteTip(ctx, r.branch); tipErr == nil && now != expected {
		return true, nil
	}

	return false, fmt.Errorf("push guidance branch: %w", pushErr)
}

// openPR creates the PR, or updates the open one's title and body.
func (r *guidanceRun) openPR(ctx context.Context, url string) (string, error) {
	if url == "" {
		created, err := r.d.prs.Create(ctx, r.in.Title, r.in.Body, r.base, r.branch)
		if err != nil {
			return "", fmt.Errorf("create PR: %w", err)
		}

		return created, nil
	}

	if err := r.d.prs.EditPR(ctx, url, r.in.Title, r.in.Body); err != nil {
		return "", fmt.Errorf("edit PR: %w", err)
	}

	return url, nil
}
