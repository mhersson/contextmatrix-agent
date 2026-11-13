package runkind

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

const urlTrim = ".,;:)]}'\"`"

var (
	urlPattern  = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s<>]+`)
	spanPattern = regexp.MustCompile("`([^`\n]+)`")
	labelRe     = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	letters     = regexp.MustCompile(`^[A-Za-z]+$`)
)

// textTokens returns the URLs, hostnames and commands text mentions, in
// first-seen order within each group, without duplicates.
func textTokens(text string) []string {
	var out []string

	add := func(tok string) {
		if tok != "" && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}

	for _, m := range urlPattern.FindAllString(text, -1) {
		u := strings.TrimRight(m, urlTrim)
		add(u)

		if parsed, err := url.Parse(u); err == nil {
			add(parsed.Hostname())
		}
	}

	for _, f := range strings.Fields(urlPattern.ReplaceAllString(text, " ")) {
		if h := strings.Trim(f, urlTrim); isHostname(h) {
			add(h)
		}
	}

	for _, c := range commandTokens(text) {
		add(c)
	}

	return out
}

// isHostname reports whether s has three or more dot-separated labels and a
// letters-only last label, so a version or a file name is not one.
func isHostname(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 3 || !letters.MatchString(labels[len(labels)-1]) {
		return false
	}

	return !slices.ContainsFunc(labels, func(l string) bool { return !labelRe.MatchString(l) })
}

// commandTokens returns each inline code span and each non-empty line of a
// fenced block, trimmed, without a leading "$ ".
func commandTokens(text string) []string {
	var out []string

	fenced := false

	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			fenced = !fenced

			continue
		}

		if fenced {
			out = append(out, cleanCommand(trimmed))

			continue
		}

		for _, m := range spanPattern.FindAllStringSubmatch(line, -1) {
			out = append(out, cleanCommand(m[1]))
		}
	}

	return out
}

func cleanCommand(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$ "))
}

// absentFromRepo returns the tokens present neither as a declared command, nor
// in tracked content at HEAD, nor in a tracked path. Hostnames match
// case-insensitively. Git runs in ws, with fixed strings and no shell.
func absentFromRepo(ctx context.Context, ws string, tokens, declared []string) ([]string, error) {
	paths, err := gitOutput(ctx, ws, "ls-files")
	if err != nil {
		return nil, fmt.Errorf("list tracked paths: %w", err)
	}

	var absent []string

	for _, tok := range tokens {
		if slices.ContainsFunc(declared, func(d string) bool { return sameCommand(tok, d) }) ||
			strings.Contains(paths, tok) {
			continue
		}

		found, err := trackedContains(ctx, ws, tok)
		if err != nil {
			return nil, err
		}

		if !found {
			absent = append(absent, tok)
		}
	}

	return absent, nil
}

// trackedContains reports whether git grep finds tok in the tracked files at
// HEAD. Exit status 1 means no match.
func trackedContains(ctx context.Context, ws, tok string) (bool, error) {
	args := []string{"grep", "-F", "-q"}
	if isHostname(tok) {
		args = append(args, "-i")
	}

	args = append(args, "-e", tok, "HEAD", "--")

	//nolint:gosec // G204: fixed git subcommand; tok follows -e as a fixed string
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = ws

	err := cmd.Run()
	if err == nil {
		return true, nil
	}

	if exitErr := (&exec.ExitError{}); errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}

	return false, fmt.Errorf("git grep: %w", err)
}

func gitOutput(ctx context.Context, ws string, args ...string) (string, error) {
	//nolint:gosec // G204: fixed git subcommand and arguments
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = ws

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}

	return string(out), nil
}
