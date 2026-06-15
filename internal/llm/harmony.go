package llm

import (
	"fmt"
	"regexp"
	"strings"
)

// Package-internal Harmony normalization. Some providers serve gpt-oss-class
// models speaking the Harmony response format: tool calls arrive as
// `<|channel|>commentary to=functions.NAME ...<|message|>{json}` segments in
// content rather than as structured tool_calls. Recipients must handle them
// (the format spec's contract); without this, such models look tool-incapable.

// harmonyCallRe matches a commentary-channel call header up through <|message|>.
// The function name is captured in group 1. The pattern allows arbitrary
// <|...|> tokens (e.g. <|constrain|>json) between the name and <|message|>.
var harmonyCallRe = regexp.MustCompile(
	`<\|channel\|>commentary to=functions\.([a-zA-Z0-9_]+)(?:\s+(?:<\|[^|>]+\|>[^\s<]*\s*)*)?<\|message\|>`)

// harmonyFramingRe strips standalone Harmony framing tokens from residual text.
var harmonyFramingRe = regexp.MustCompile(
	`<\|start\|>assistant|<\|end\|>|<\|return\|>`)

// harmonyFinalChannelRe matches a final-channel segment; group 1 is the message
// text to preserve as prose (final channel is informational, not a tool call).
var harmonyFinalChannelRe = regexp.MustCompile(
	`<\|channel\|>final<\|message\|>([^<]*)`)

// harmonyTokenRe strips leftover bare <|...|>word pairs after other replacements.
var harmonyTokenRe = regexp.MustCompile(`<\|[a-zA-Z0-9_]+\|>[^\s<]*`)

// extractHarmonyToolCalls pulls Harmony-format tool calls out of content.
// Returns the calls (nil if none) and the content with call segments and
// Harmony framing tokens removed. Arguments run to the next <|...|> token or
// end of string; unparseable JSON is left for the loop's repair budget.
func extractHarmonyToolCalls(content string) ([]ToolCall, string) {
	matches := harmonyCallRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		// No commentary calls — still strip framing tokens from rest.
		rest := stripHarmonyFraming(content)

		return nil, rest
	}

	var (
		calls     []ToolCall
		restParts []string
	)

	pos := 0

	for i, m := range matches {
		matchStart := m[0] // start of <|channel|>commentary...
		matchEnd := m[1]   // end of <|message|>
		funcName := content[m[2]:m[3]]

		// Prose before this match is part of the residual.
		if matchStart > pos {
			restParts = append(restParts, content[pos:matchStart])
		}

		// Arguments: from matchEnd to the next <|channel|> (next call) or end.
		// If there are more matches, the next call starts at matches[i+1][0].
		var argEnd int
		if i+1 < len(matches) {
			argEnd = matches[i+1][0]
		} else {
			// Last call: arguments run to the next <| token or end of string.
			nextToken := strings.Index(content[matchEnd:], "<|")
			if nextToken >= 0 {
				argEnd = matchEnd + nextToken
			} else {
				argEnd = len(content)
			}
		}

		args := strings.TrimSpace(content[matchEnd:argEnd])
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("harmony-%d", len(calls)),
			Type: "function",
			Function: FunctionCall{
				Name:      funcName,
				Arguments: args,
			},
		})

		pos = argEnd
	}

	// Anything after the last argument (trailing framing tokens, etc.).
	if pos < len(content) {
		restParts = append(restParts, content[pos:])
	}

	rest := strings.Join(restParts, "")
	rest = stripHarmonyFraming(rest)

	return calls, rest
}

// stripHarmonyFraming removes Harmony framing tokens and stray <|...|> pairs
// from s, returning the trimmed prose residual. The final-channel message text
// is preserved (it is informational prose, not a tool call). Framing-like
// tokens appearing literally inside final-channel prose are also stripped by
// the token pass — such input is malformed and this is intentional; do not
// reorder the passes to "fix" it.
func stripHarmonyFraming(s string) string {
	// Replace <|channel|>final<|message|>TEXT with just TEXT.
	s = harmonyFinalChannelRe.ReplaceAllString(s, "$1")
	s = harmonyFramingRe.ReplaceAllString(s, "")
	s = harmonyTokenRe.ReplaceAllString(s, "")

	return strings.TrimSpace(s)
}

// normalizeHarmony applies extraction to a Response when it has no
// structured tool calls but content carries Harmony markers.
func normalizeHarmony(resp Response) Response {
	if len(resp.ToolCalls) > 0 || !strings.Contains(resp.Content, "<|channel|>") {
		return resp
	}

	calls, rest := extractHarmonyToolCalls(resp.Content)
	resp.Content = rest

	if len(calls) > 0 {
		resp.ToolCalls = calls
		if resp.FinishReason == "stop" || resp.FinishReason == "" {
			resp.FinishReason = "tool_calls"
		}
	}

	return resp
}
