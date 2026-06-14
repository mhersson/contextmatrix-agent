package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

const (
	// readMaxLines is the default page size when no limit is specified.
	readMaxLines = 2000
	// readMaxBytes is the byte ceiling per response — deliberately under the
	// 128 KB harness backstop so read output is never re-truncated downstream.
	readMaxBytes = 120_000
	// sniffSize is the number of bytes inspected for binary detection.
	sniffSize = 8192
)

// looksBinary reports whether p appears to be binary content.
// It uses the same NUL-byte heuristic as git.
func looksBinary(p []byte) bool {
	return bytes.IndexByte(p, 0) >= 0
}

type ReadTool struct{ root string }

func NewReadTool(root string) ReadTool { return ReadTool{root: root} }

func (t ReadTool) Name() string { return "read" }

func (t ReadTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "read",
		Description: "Read a UTF-8 text file. Large files are paginated: " +
			"by default up to 2000 lines or 120 KB are returned. " +
			"Use offset (1-based) and limit to page through larger files. " +
			"Binary files are detected and reported without loading content into context.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"file path relative to the workspace root"},
				"offset":{"type":"integer","description":"optional 1-based first line to return; use the hint in the previous response to continue"},
				"limit":{"type":"integer","description":"optional maximum number of lines"}
			},
			"required":["path"]
		}`),
	}}
}

func (t ReadTool) Execute(_ context.Context, args map[string]any) (string, error) {
	rel, err := requireString(args, "path")
	if err != nil {
		return "", err
	}

	abs, err := resolveInRoot(t.root, rel)
	if err != nil {
		return "", err
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", rel, err)
	}

	if fi.IsDir() {
		return "", fmt.Errorf("read %s: is a directory", rel)
	}

	// Sniff for binary content without reading the whole file. A short or
	// empty read returns io.EOF, which is not an error here — empty files
	// (__init__.py, .gitkeep) flow through to return "".
	sniff := make([]byte, sniffSize)

	n, err := f.Read(sniff)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}

	sniff = sniff[:n]

	if looksBinary(sniff) {
		return fmt.Sprintf("[binary file: %s, %d bytes — not shown]", rel, fi.Size()), nil
	}

	// Text path: rewind and read the whole file fresh (the sniff consumed the
	// first chunk). Avoids aliasing the sniff buffer's backing array.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}

	offset := optInt(args, "offset", 0)
	limit := optInt(args, "limit", 0)

	lines := strings.SplitAfter(string(b), "\n")
	totalLines := len(lines)

	// SplitAfter("a\nb\n", "\n") produces ["a\n","b\n",""] — the trailing empty
	// string is an artifact of the final newline, not a real line. Track the
	// logical end so we don't fire a spurious "more content" hint.
	logicalEnd := totalLines
	if totalLines > 0 && lines[totalLines-1] == "" {
		logicalEnd = totalLines - 1
	}

	start := 0
	if offset > 0 {
		start = offset - 1
	}

	if start > totalLines {
		start = totalLines
	}

	// Determine the page size: explicit limit if positive, else the default.
	// Cap the window relative to remaining lines without computing start+page
	// directly, so a pathological limit (e.g. a huge JSON float) can't overflow
	// int and wrap negative — it simply acts as "read to end".
	page := readMaxLines
	if limit > 0 {
		page = limit
	}

	end := totalLines
	if remaining := totalLines - start; page < remaining {
		end = start + page
	}

	// Byte ceiling: walk lines[start:end] and stop at the last whole line
	// that keeps total bytes ≤ readMaxBytes.
	byteCapped := false
	byteCount := 0
	cappedEnd := start

	for i := start; i < end; i++ {
		lineLen := len(lines[i])
		if i == start && lineLen > readMaxBytes {
			// Even the first line alone exceeds the byte ceiling — truncate it.
			cappedEnd = i + 1
			byteCapped = true

			break
		}

		if byteCount+lineLen > readMaxBytes {
			break
		}

		byteCount += lineLen
		cappedEnd = i + 1
	}

	// If no lines fit (e.g. start == end), return empty with no hint.
	if cappedEnd == start && !byteCapped {
		return "", nil
	}

	var out strings.Builder

	if byteCapped {
		// Write only readMaxBytes of the over-long first line.
		out.WriteString(lines[start][:readMaxBytes])

		lineNum := start + 1
		lineEnd := cappedEnd // == start+1
		// The truncated remainder of this line (bytes readMaxBytes..end) is NOT
		// reachable via the line-based offset API — only subsequent lines are.
		// Be honest: never imply the line's bytes continue at the offered offset.
		if lineEnd < logicalEnd {
			fmt.Fprintf(&out, "\n[line %d truncated: %d of %d bytes shown; the rest of this line is not retrievable — call read with offset=%d for the following lines]",
				lineNum, readMaxBytes, len(lines[start]), lineEnd+1)
		} else {
			fmt.Fprintf(&out, "\n[line %d truncated: %d of %d bytes shown; the rest is not retrievable]",
				lineNum, readMaxBytes, len(lines[start]))
		}
	} else {
		out.WriteString(strings.Join(lines[start:cappedEnd], ""))

		// Append pagination hint when the default page cap kicked in (no
		// explicit limit was given) and more real content remains. When the
		// caller supplied an explicit limit, they asked for that slice
		// intentionally — no hint needed.
		if limit <= 0 && cappedEnd < logicalEnd {
			lastLine := cappedEnd // lines are 0-indexed; cappedEnd is exclusive
			fmt.Fprintf(&out, "\n[showing lines %d-%d of %d; call read with offset=%d to continue]",
				start+1, lastLine, logicalEnd, lastLine+1)
		}
	}

	return out.String(), nil
}
