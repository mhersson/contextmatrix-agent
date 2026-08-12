package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogCardWarnsOnAddLogFailure proves a rejected board write is no longer
// silent: it must never abort the run (advisory only), but it must now show up
// somewhere - the container's per-card file log captures stderr - carrying the
// card ID and the dropped message.
func TestLogCardWarnsOnAddLogFailure(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ops := &fakeOps{addLogErr: errors.New("add_log: card CARD-1 is not claimed; add_log requires an active claim")}
	d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}

	require.NotPanics(t, func() {
		d.logCard(context.Background(), "run complete: %q integrated and pushed", "All done")
	}, "a rejected write must never abort the run")

	out := buf.String()
	assert.Contains(t, out, "add_log failed")
	assert.Contains(t, out, "CARD-1")
	assert.Contains(t, out, "run complete")
	assert.Contains(t, out, "not claimed")
}

// TestLogCardTruncatesLongMessageInWarning proves the warned message is capped
// so a large advisory line (a completion note, a verify tail) does not spam
// the process log with what the board never got anyway.
func TestLogCardTruncatesLongMessageInWarning(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ops := &fakeOps{addLogErr: errors.New("boom")}
	d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}

	long := strings.Repeat("x", logMessageHeadMax*2)

	d.logCard(context.Background(), "%s", long)

	out := buf.String()
	assert.Contains(t, out, strings.Repeat("x", logMessageHeadMax)+"...")
	assert.NotContains(t, out, strings.Repeat("x", logMessageHeadMax+1),
		"the warned message must be capped, not the full dropped note")
}

// TestLogCardSilentOnSuccess proves the happy path stays quiet: a successful
// AddLog must not warn.
func TestLogCardSilentOnSuccess(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ops := &fakeOps{}
	d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}

	d.logCard(context.Background(), "verify passed")

	assert.Empty(t, buf.String(), "a successful add_log must not warn")
	assert.True(t, ops.loggedContains("verify passed"))
}
