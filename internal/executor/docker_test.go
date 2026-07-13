package executor

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
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

func TestContainerConfig_CACertMountAndEnv(t *testing.T) {
	cfg, host := containerConfig(LaunchSpec{
		CardID:         "ABC-1",
		Project:        "demo",
		Image:          "alpine:3",
		Env:            []string{"FOO=bar"},
		CACertHostFile: "/etc/cm/extra-ca.pem",
	})

	assert.Contains(t, host.Binds, "/etc/cm/extra-ca.pem:/run/cm-ca/ca.crt:ro")
	assert.Contains(t, cfg.Env, "CMX_CA_CERT_FILE=/run/cm-ca/ca.crt")
	assert.Contains(t, cfg.Env, "FOO=bar", "pre-existing env is preserved")

	// The git/gh CA vars are deliberately NOT set at the container level — the
	// harness scrubs subprocess env, so they would be dead and misleading. git/gh
	// get them on their explicit subprocess env instead.
	for _, e := range cfg.Env {
		assert.NotContains(t, e, "GIT_SSL_CAINFO")
		assert.NotContains(t, e, "GH_CA_BUNDLE")
		assert.NotContains(t, e, "SSL_CERT_FILE")
	}
}

func TestContainerConfig_NoCACertByDefault(t *testing.T) {
	cfg, host := containerConfig(LaunchSpec{
		CardID:  "ABC-1",
		Project: "demo",
		Image:   "alpine:3",
		Env:     []string{"FOO=bar"},
	})

	for _, b := range host.Binds {
		assert.NotContains(t, b, "/run/cm-ca/")
	}

	for _, e := range cfg.Env {
		assert.NotContains(t, e, "CMX_CA_CERT_FILE")
	}
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

func TestContainerConfigMountsSkillsReadOnly(t *testing.T) {
	_, host := containerConfig(LaunchSpec{
		CardID:         "CARD-1",
		Project:        "proj",
		Image:          "img",
		SecretsHostDir: "/host/secrets",
		SkillsHostDir:  "/host/skills",
	})

	assert.Contains(t, host.Binds, "/host/secrets:/run/cm-secrets:ro")
	assert.Contains(t, host.Binds, "/host/skills:/run/cm-skills:ro", "skills dir is bound read-only")
}

func TestContainerConfigNoSkillsBindWhenUnset(t *testing.T) {
	_, host := containerConfig(LaunchSpec{CardID: "CARD-1", Project: "proj", Image: "img"})

	for _, b := range host.Binds {
		assert.NotContains(t, b, "/run/cm-skills", "no skills bind when SkillsHostDir is empty")
	}
}

func TestNewDockerExecutor_WiresOnStart(t *testing.T) {
	called := false
	e := NewDockerExecutor(Config{
		OnStart: func(_, _, _ string) { called = true },
	})

	require.NotNil(t, e.onStart)

	e.onStart("p", "c", "id")
	assert.True(t, called)
}

type fakeWaiter struct{ exits bool }

func (f *fakeWaiter) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	wc := make(chan container.WaitResponse, 1)
	ec := make(chan error, 1)

	if f.exits {
		wc <- container.WaitResponse{StatusCode: 0}
	} else {
		go func() {
			<-ctx.Done()

			ec <- ctx.Err()
		}()
	}

	return wc, ec
}

func TestWaitForSelfExitExited(t *testing.T) {
	assert.True(t, waitForSelfExit(t.Context(), &fakeWaiter{exits: true}, "c1", time.Second))
}

func TestWaitForSelfExitTimesOut(t *testing.T) {
	start := time.Now()

	assert.False(t, waitForSelfExit(t.Context(), &fakeWaiter{exits: false}, "c1", 20*time.Millisecond))
	assert.Less(t, time.Since(start), time.Second, "must give up at the grace bound")
}

func TestImageSummaries_SkipsDanglingAndMapsFields(t *testing.T) {
	in := []image.Summary{
		{
			RepoTags:    []string{"contextmatrix-agent-worker:go-node"},
			RepoDigests: []string{"contextmatrix-agent-worker@sha256:abc"},
			Created:     1750000000,
			Size:        2_560_000_000,
		},
		{RepoTags: nil, RepoDigests: []string{"orphan@sha256:def"}},     // dangling: skipped
		{RepoTags: []string{"<none>:<none>"}},                           // dangling tag form: skipped
		{RepoTags: []string{"other:latest", "<none>:<none>"}, Size: 42}, // <none> pruned, image kept
	}

	got := imageSummaries(in)

	require.Len(t, got, 2)
	assert.Equal(t, ImageSummary{
		Tags:      []string{"contextmatrix-agent-worker:go-node"},
		Digests:   []string{"contextmatrix-agent-worker@sha256:abc"},
		CreatedAt: 1750000000,
		SizeBytes: 2_560_000_000,
	}, got[0])
	assert.Equal(t, []string{"other:latest"}, got[1].Tags)
	assert.Equal(t, int64(42), got[1].SizeBytes)
}
