// Package redact masks known literal secret values in strings before they
// reach the model, the event stream, transcripts, or log frames.
package redact

import (
	"sort"
	"strings"
)

const (
	mask = "[REDACTED]"
	// minLen guards against mangling text with trivially short "secrets".
	minLen = 6
)

// Redactor replaces known literal secret values with a fixed mask.
type Redactor struct{ replacer *strings.Replacer }

// New builds a redactor over the given literal values. Empty and very short
// values are dropped. Values are sorted longest-first so that when two secrets
// share a start position the longer is listed first and wins Replacer's
// first-match-wins tie-break; otherwise the shorter would match and leave the
// tail exposed.
func New(secrets []string) *Redactor {
	vals := make([]string, 0, len(secrets))

	for _, s := range secrets {
		if len(s) >= minLen {
			vals = append(vals, s)
		}
	}

	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })

	pairs := make([]string, 0, len(vals)*2)
	for _, v := range vals {
		pairs = append(pairs, v, mask)
	}

	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// Apply masks all known secrets. A nil receiver is the identity.
func (r *Redactor) Apply(s string) string {
	if r == nil || r.replacer == nil || s == "" {
		return s
	}

	return r.replacer.Replace(s)
}
