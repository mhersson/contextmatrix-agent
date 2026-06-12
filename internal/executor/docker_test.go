package executor

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerConfig_StdinAndImage(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "alpine:3", cfg.Image)
	assert.True(t, cfg.OpenStdin, "OpenStdin must be set so control frames can be written")
	assert.True(t, cfg.AttachStdin)
	assert.False(t, cfg.StdinOnce, "stdin stays open for the container's life")
	assert.False(t, cfg.Tty)
}

func TestContainerConfig_Labels(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "true", cfg.Labels[labelAgent])
	assert.Equal(t, "ABC-1", cfg.Labels[labelCardID])
	assert.Equal(t, "demo", cfg.Labels[labelProject])
	// Correlation ID label is omitted when empty.
	_, ok := cfg.Labels[labelCorrelationID]
	assert.False(t, ok)
}

func TestContainerConfig_CorrelationIDLabel(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:        "ABC-1",
		Project:       "demo",
		Image:         "alpine:3",
		CorrelationID: "corr-123",
	})

	assert.Equal(t, "corr-123", cfg.Labels[labelCorrelationID])
}

func TestContainerConfig_EnvPassthrough(t *testing.T) {
	env := []string{"FOO=bar", "BAZ=qux"}

	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
		Env:     env,
	})

	assert.Equal(t, env, cfg.Env)
}

func TestContainerConfig_HostConfigResourcesAndHardening(t *testing.T) {
	const (
		mem  = int64(8 * 1024 * 1024 * 1024)
		pids = int64(512)
	)

	_, host := containerConfig(LaunchSpec{
		CardID:         "ABC-1",
		Project:        "demo",
		Image:          "alpine:3",
		SecretsHostDir: "/srv/cm/secrets/demo",
		MemoryBytes:    mem,
		PidsLimit:      pids,
	})

	assert.Equal(t, mem, host.Memory)

	require.NotNil(t, host.PidsLimit)
	assert.Equal(t, pids, *host.PidsLimit)

	assert.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	assert.Equal(t, []string{"no-new-privileges"}, host.SecurityOpt)
	assert.Equal(t, []string{"/srv/cm/secrets/demo:/run/cm-secrets:ro"}, host.Binds)
}

func TestContainerConfig_RunsAsNonRoot(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Equal(t, "1000:1000", cfg.User)
}

func TestContainerConfig_NoBindsWhenSecretsDirEmpty(t *testing.T) {
	_, host := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
	})

	assert.Empty(t, host.Binds)
}

func TestContainerName_Sanitized(t *testing.T) {
	tests := []struct {
		name    string
		project string
		cardID  string
		want    string
	}{
		{
			name:    "lowercases card id",
			project: "demo",
			cardID:  "ABC-1",
			want:    "cm-agent-demo-abc-1",
		},
		{
			name:    "sanitizes disallowed chars in project",
			project: "my/proj@v2",
			cardID:  "ABC-1",
			want:    "cm-agent-my-proj-v2-abc-1",
		},
		{
			name:    "spaces become dashes",
			project: "team alpha",
			cardID:  "X-9",
			want:    "cm-agent-team-alpha-x-9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containerName(tt.project, tt.cardID))
		})
	}
}

func TestContainerName_MatchesDockerCharset(t *testing.T) {
	got := containerName("my/proj@v2", "ABC-1")
	assert.Regexp(t, `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`, got)
}

// DockerExecutor must satisfy the Executor seam.
var _ Executor = (*DockerExecutor)(nil)

func TestLineWriter_SplitsOnNewline(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	n, err := w.Write([]byte("alpha\nbeta\n"))
	require.NoError(t, err)
	assert.Equal(t, len("alpha\nbeta\n"), n)
	assert.Equal(t, []string{"alpha", "beta"}, got)
}

func TestLineWriter_PartialLineHeldUntilFlush(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("hel"))
	_, _ = w.Write([]byte("lo\nwor"))

	assert.Equal(t, []string{"hello"}, got, "complete line emitted, partial held")

	w.Flush()
	assert.Equal(t, []string{"hello", "wor"}, got, "flush emits the trailing partial line")
}

func TestLineWriter_FlushOnEmptyBufferIsNoop(t *testing.T) {
	called := false

	w := newLineWriter(func([]byte) { called = true })
	w.Flush()

	assert.False(t, called)
}

func TestLineWriter_TrimsCarriageReturn(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("windows\r\nline\r\n"))

	assert.Equal(t, []string{"windows", "line"}, got)
}

func TestLineWriter_BoundsLongLine(t *testing.T) {
	var got []byte

	w := newLineWriter(func(line []byte) {
		got = append([]byte(nil), line...)
	})

	huge := bytes.Repeat([]byte("x"), scannerBufferMax+4096)
	_, _ = w.Write(huge)
	w.Flush()

	assert.LessOrEqual(t, len(got), scannerBufferMax,
		"line buffer must not grow past the cap")
}
