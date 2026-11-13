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
	urlPattern   = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s<>]+`)
	spanPattern  = regexp.MustCompile("`{1,2}([^`\n]+)`{1,2}")
	fieldPattern = regexp.MustCompile(`\S+`)
	labelRe      = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	letters      = regexp.MustCompile(`^[A-Za-z]+$`)
)

// textTokens returns the URLs, hostnames and commands text mentions, in
// first-seen order, without duplicates (hostnames compare case-insensitively).
func textTokens(text string) []string {
	var found []positioned

	found = append(found, urlTokens(text)...)
	found = append(found, hostTokens(text)...)
	found = append(found, commandTokens(text)...)

	slices.SortStableFunc(found, func(a, b positioned) int { return a.pos - b.pos })

	var out []string

	seen := map[string]bool{}

	for _, f := range found {
		key := f.tok
		if isHostname(key) {
			key = strings.ToLower(key)
		}

		if f.tok == "" || seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, f.tok)
	}

	return out
}

// positioned is a token and its byte offset in the text.
type positioned struct {
	pos int
	tok string
}

// urlTokens returns each URL and its host.
func urlTokens(text string) []positioned {
	var out []positioned

	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		u := strings.TrimRight(text[loc[0]:loc[1]], urlTrim)
		out = append(out, positioned{loc[0], u})

		if parsed, err := url.Parse(u); err == nil {
			out = append(out, positioned{loc[0], parsed.Hostname()})
		}
	}

	return out
}

// hostTokens returns the bare hostnames outside URLs, also when written with a
// user@ prefix, a port or a path.
func hostTokens(text string) []positioned {
	blanked := urlPattern.ReplaceAllStringFunc(text, func(m string) string { return strings.Repeat(" ", len(m)) })

	var out []positioned

	for _, loc := range fieldPattern.FindAllStringIndex(blanked, -1) {
		f := strings.Trim(blanked[loc[0]:loc[1]], urlTrim+"([{<")
		if i := strings.Index(f, "@"); i >= 0 {
			f = f[i+1:]
		}

		if i := strings.IndexAny(f, ":/"); i >= 0 {
			f = f[:i]
		}

		if isHostname(f) {
			out = append(out, positioned{loc[0], f})
		}
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
func commandTokens(text string) []positioned {
	var out []positioned

	fenced := false
	offset := 0

	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fenced = !fenced
		case fenced:
			out = append(out, positioned{offset, cleanCommand(trimmed)})
		default:
			for _, m := range spanPattern.FindAllStringSubmatchIndex(line, -1) {
				out = append(out, positioned{offset + m[0], cleanCommand(line[m[2]:m[3]])})
			}
		}

		offset += len(line)
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
	//nolint:gosec // G204: fixed git arguments
	list := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", "-z", "HEAD")
	list.Dir = ws

	out, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("list tracked paths: %w", err)
	}

	paths := string(out)

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
