package runkind

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cardid"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"gopkg.in/yaml.v3"
)

// writeInputs writes a model kind's card files under root, read-only: card.md,
// and subtasks/<id>.md and siblings/<id>.md, named <n>.md (1-based) when a file
// has no card ID. The run log stays where serve mounted it.
func writeInputs(root, card string, subtasks, siblings []string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}

	if err := writeReadOnly(filepath.Join(root, "card.md"), card); err != nil {
		return err
	}

	if err := writeCards(filepath.Join(root, "subtasks"), subtasks); err != nil {
		return err
	}

	return writeCards(filepath.Join(root, "siblings"), siblings)
}

func writeCards(dir string, files []string) error {
	if len(files) == 0 {
		return nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	for i, f := range files {
		// The ID names a file, so only a card ID's safe shape is used.
		name := cardFileID(f)
		if !cardid.Valid(name) {
			name = strconv.Itoa(i + 1)
		}

		if err := writeReadOnly(filepath.Join(dir, name+".md"), f); err != nil {
			return err
		}
	}

	return nil
}

func writeReadOnly(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o444); err != nil { //nolint:gosec // G306: read-only card files, no secrets
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// cardFileID is the id of a card file's YAML frontmatter; "" when absent.
func cardFileID(file string) string {
	rest, ok := strings.CutPrefix(file, "---\n")
	if !ok {
		return ""
	}

	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}

	var fm struct {
		ID string `yaml:"id"`
	}

	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return ""
	}

	return fm.ID
}

// fetchWorkBranch fetches the card's work branch into refs/remotes/origin/ and
// returns its name; "" when the remote has none.
func fetchWorkBranch(ctx context.Context, spec Spec, ws string) (string, error) {
	git := worker.NewGit(ws, spec.SecretsPath, worker.HostFromRepoURL(spec.RepoURL), spec.CACertFile)

	heads, err := git.RemoteHeads(ctx, cardBranch(spec.CardID))
	if err != nil {
		return "", fmt.Errorf("list work branches: %w", err)
	}

	branch := workBranch(spec.CardID, heads)
	if branch == "" {
		return "", nil
	}

	if err := fetchBranch(ctx, git, branch); err != nil {
		return "", fmt.Errorf("fetch work branch: %w", err)
	}

	return branch, nil
}

// guidanceBranch is the project's standing guidance PR head.
func guidanceBranch(project string) string {
	return protocol.GuidanceBranchPrefix + project
}

// fetchGuidanceBranch fetches the project's guidance branch into
// refs/remotes/origin/ and returns its name; "" when the remote has none.
func fetchGuidanceBranch(ctx context.Context, spec Spec, ws string) (string, error) {
	git := worker.NewGit(ws, spec.SecretsPath, worker.HostFromRepoURL(spec.RepoURL), spec.CACertFile)
	branch := guidanceBranch(spec.Project)

	tip, err := git.RemoteTip(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("look up guidance branch: %w", err)
	}

	if tip == "" {
		return "", nil
	}

	if err := fetchBranch(ctx, git, branch); err != nil {
		return "", fmt.Errorf("fetch guidance branch: %w", err)
	}

	return branch, nil
}

func fetchBranch(ctx context.Context, git *worker.Git, branch string) error {
	return git.Fetch(ctx, "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
}

// cardBranch is the card's plain work branch in the worker's cm/ namespace.
func cardBranch(cardID string) string {
	return "cm/" + strings.ToLower(cardID)
}

// workBranch picks the card's work branch among heads: the plain branch, else
// the one branch suffixed with a Jira key; "" when neither is listed.
func workBranch(cardID string, heads []string) string {
	plain := cardBranch(cardID)

	var keyed []string

	for _, h := range heads {
		if h == plain {
			return plain
		}

		if key, ok := strings.CutPrefix(h, plain+"-"); ok && worker.BranchKey(key) {
			keyed = append(keyed, h)
		}
	}

	if len(keyed) == 1 {
		return keyed[0]
	}

	return ""
}
