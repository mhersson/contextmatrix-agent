package redact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactor(t *testing.T) {
	r := New([]string{"sk-or-longsecret123", "ghs_tok", "", "ab"}) // empty + short ignored

	assert.Equal(t, "key=[REDACTED] t=[REDACTED]", r.Apply("key=sk-or-longsecret123 t=ghs_tok"))
	assert.Equal(t, "plain ab text", r.Apply("plain ab text")) // <6 chars not masked
	assert.Empty(t, r.Apply(""))

	// longest-first listing wins Replacer's first-match-wins tie-break at a
	// shared start position, so overlapping secrets don't leave residue
	r2 := New([]string{"secret", "secret-extended"})
	assert.Equal(t, "[REDACTED]", r2.Apply("secret-extended"))
}

func TestNilRedactorIsIdentity(t *testing.T) {
	var r *Redactor
	assert.Equal(t, "x", r.Apply("x"))
}
