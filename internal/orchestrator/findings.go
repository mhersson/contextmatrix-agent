package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// maxFindings and maxFindingsBytes bound how much recorded context a single run
// can accumulate. Both are stated back to the model when reached rather than
// silently dropping the entry, so a truncated list never reads as a complete one.
const (
	maxFindings      = 60
	maxFindingsBytes = 8192
)

// FindingsTool is a durable scratchpad for a model run. The harness carries only
// message content and tool calls between turns, so a model's reasoning - and
// every conclusion inside it - is discarded each turn. Recording a conclusion
// here puts it in a tool result, which does survive, and every call echoes the
// full list so the newest result always carries everything established so far.
//
// The tool is phase-agnostic by construction: registering it for another phase
// must be a registry and prompt change only.
type FindingsTool struct {
	entries []string
	seen    map[string]bool
	nbytes  int
}

func NewFindingsTool() *FindingsTool {
	return &FindingsTool{seen: map[string]bool{}}
}

func (t *FindingsTool) Name() string { return "record_finding" }

func (t *FindingsTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "record_finding",
		Description: "Record a conclusion or a confirmed code anchor so it survives into your later turns. " +
			"Call it as soon as you establish something you will rely on. The result returns every finding " +
			"recorded so far - consult that list before re-reading a file you have already inspected.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"fact":{"type":"string","description":"the conclusion, stated so it is useful without re-reading the source"},
				"anchor":{"type":"string","description":"optional location the fact was confirmed at, such as a file and line or a symbol name"}
			},
			"required":["fact"]
		}`),
	}}
}

func (t *FindingsTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	fact, ok := args["fact"].(string)
	if !ok || strings.TrimSpace(fact) == "" {
		return tools.Result{Text: `record_finding needs a non-empty "fact" string argument. ` + t.render()}, nil
	}

	anchor, _ := args["anchor"].(string)

	entry := strings.TrimSpace(fact)
	if a := strings.TrimSpace(anchor); a != "" {
		entry = a + " - " + entry
	}

	if key := normalizeFinding(entry); !t.seen[key] {
		switch {
		case len(t.entries) >= maxFindings:
			return tools.Result{Text: fmt.Sprintf(
				"cap reached: %d findings already recorded, this one was not added. %s",
				maxFindings, t.render())}, nil
		case t.nbytes+len(entry) > maxFindingsBytes:
			return tools.Result{Text: fmt.Sprintf(
				"cap reached: recorded findings would exceed %d bytes, this one was not added. %s",
				maxFindingsBytes, t.render())}, nil
		default:
			t.seen[key] = true
			t.entries = append(t.entries, entry)
			t.nbytes += len(entry)
		}
	}

	return tools.Result{Text: t.render()}, nil
}

func (t *FindingsTool) render() string {
	var b strings.Builder

	fmt.Fprintf(&b, "RECORDED FINDINGS (%d)\n", len(t.entries))

	for i, e := range t.entries {
		fmt.Fprintf(&b, "%d. %s\n", i+1, e)
	}

	return b.String()
}

// normalizeFinding is the dedupe key: case-folded with runs of whitespace
// collapsed, so the same fact recorded twice with different wrapping or
// capitalisation does not grow the list.
func normalizeFinding(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

var _ tools.Tool = (*FindingsTool)(nil)
