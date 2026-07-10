package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/kata"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// solverLLM scripts a model that rewrites rle.go with the correct Encode via
// edit, runs the tests, then stops — exercising the whole wiring offline.
type solverLLM struct {
	root string
	step int
}

func (s *solverLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return s.SendStream(ctx, req, nil)
}

func (s *solverLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	s.step++
	good := `package kata
import "strconv"
func Encode(s string) string {
	if s == "" { return "" }
	r := []rune(s); out := ""; count := 1
	for i := 1; i <= len(r); i++ {
		if i < len(r) && r[i] == r[i-1] { count++; continue }
		out += strconv.Itoa(count) + string(r[i-1]); count = 1
	}
	return out
}`

	switch s.step {
	case 1:
		// Read the actual skeleton and replace the whole file in one valid edit.
		cur, err := os.ReadFile(filepath.Join(s.root, "rle.go"))
		if err != nil {
			return llm.Response{}, err
		}

		return llm.Response{ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function",
			Function: llm.FunctionCall{Name: "edit", Arguments: mustJSON(map[string]any{
				"path": "rle.go", "old_string": string(cur), "new_string": good,
			})},
		}}}, nil
	case 2:
		return llm.Response{ToolCalls: []llm.ToolCall{{
			ID: "2", Type: "function",
			Function: llm.FunctionCall{Name: "bash", Arguments: `{"command":"go test ./..."}`},
		}}}, nil
	default:
		return llm.Response{Content: "tests pass", FinishReason: "stop"}, nil
	}
}

func TestRunSpikeDrivesKataGreen(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	dir := t.TempDir()
	require.NoError(t, kata.Copy(dir))

	res, err := runSpike(context.Background(), &solverLLM{root: dir}, runOpts{
		taskDir: dir, task: "Make the failing test pass.", maxTurns: 10,
	})
	require.NoError(t, err)
	assert.True(t, res.Completed)

	// The kata must now actually pass.
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	require.NoError(t, cmd.Run(), "kata should be green after the run")

	b, _ := os.ReadFile(filepath.Join(dir, "rle.go"))
	assert.Contains(t, string(b), "strconv")
}

func mustJSON(m map[string]any) string {
	b, _ := json.Marshal(m)

	return string(b)
}

// TestToolOutputMaxDefaultIs131072 asserts that the --tool-output-max-bytes flag
// defaults to 131072 (128 KB) — matching the FSM / RunSpec default so local runs
// and autonomous runs cap tool output identically.
func TestToolOutputMaxDefaultIs131072(t *testing.T) {
	cmd := newRunCmd()
	v, err := cmd.Flags().GetInt("tool-output-max-bytes")
	require.NoError(t, err)
	assert.Equal(t, 131072, v)
}

func TestPrintConfigSkipsValidation(t *testing.T) {
	cmd := newRunCmd()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	// max-turns=0 is invalid, but --print-config must still succeed and print it.
	cmd.SetArgs([]string{"--print-config", "--max-turns=0"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "max-turns: 0")
}

func TestCommandCheckPassAndFail(t *testing.T) {
	root := t.TempDir()

	v, err := commandCheck(root, "true")(context.Background())
	require.NoError(t, err)
	assert.True(t, v.OK)

	v, err = commandCheck(root, "echo nope >&2; exit 1")(context.Background())
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.Contains(t, v.Detail, "nope")
}

func TestCommandCheckUnrunnableErrors(t *testing.T) {
	root := t.TempDir()

	_, err := commandCheck(root, "definitely-not-a-real-tool-xyz --check")(context.Background())
	require.Error(t, err, "an unrunnable declared command is a loud error, not a fake pass")
	assert.Contains(t, err.Error(), "cannot run")
}

func TestCommandCheckInheritsEnv(t *testing.T) {
	// The standalone `run --verify` runs OUTSIDE the container trust boundary, so
	// it inherits the developer's full environment — a scrubbed allowlist would
	// silently drop DATABASE_URL etc. and break integration-style verify commands.
	t.Setenv("VERIFY_SMOKE_VAR", "inherited-value")

	root := t.TempDir()

	v, err := commandCheck(root, "echo $VERIFY_SMOKE_VAR")(context.Background())
	require.NoError(t, err)
	assert.True(t, v.OK)
	assert.Contains(t, v.Detail, "inherited-value", "the CLI verify command must see the caller's environment")
}
