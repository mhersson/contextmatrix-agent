package filelog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNoSection reports that a card's log holds no section for the run.
var ErrNoSection = errors.New("filelog: run section not found")

const (
	// sectionCapBytes bounds one copied section; sectionHeadBytes of it is the
	// section's start, the rest its tail.
	sectionCapBytes  = 256 << 20
	sectionHeadBytes = 32 << 20

	runFooterPrefix = "==== run ended "
)

// CopySection writes the run's section of the card's log to w: its header line
// through its footer, or through the end of the file while the run still writes.
func (l *Logger) CopySection(project, cardID, runID string, w io.Writer) error {
	if !l.enabled() {
		return ErrNoSection
	}

	f, err := os.Open(l.path(project, cardID))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoSection
	}

	if err != nil {
		return fmt.Errorf("open card log: %w", err)
	}

	defer f.Close()

	start, end, err := findSection(f, runID)
	if err != nil {
		return err
	}

	return copySection(w, f, start, end, sectionCapBytes, sectionHeadBytes)
}

// findSection returns the byte range of the last header naming the run through
// its first later footer, or to the end of r when there is none. The id is
// matched as a whole field.
func findSection(r io.Reader, runID string) (start, end int64, err error) {
	field := []byte(" run=" + runID + " ")
	br := bufio.NewReader(r)

	var (
		pos   int64
		found bool
		open  bool
	)

	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			next := pos + int64(len(line))

			switch {
			case bytes.HasPrefix(line, []byte(runHeaderPrefix)) && bytes.Contains(line, field):
				start, end, found, open = pos, 0, true, true
			case open && bytes.HasPrefix(line, []byte(runFooterPrefix)) && bytes.Contains(line, field):
				end, open = next, false
			}

			pos = next
		}

		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			return 0, 0, fmt.Errorf("scan card log: %w", readErr)
		}
	}

	if !found {
		return 0, 0, ErrNoSection
	}

	if open {
		end = pos
	}

	return start, end, nil
}

// copySection streams ra[start:end] to w. A range over capBytes keeps its first
// headBytes, a cut marker, then enough of its tail to fill capBytes.
func copySection(w io.Writer, ra io.ReaderAt, start, end, capBytes, headBytes int64) error {
	size := end - start
	if size <= capBytes {
		if _, err := io.Copy(w, io.NewSectionReader(ra, start, size)); err != nil {
			return fmt.Errorf("copy section: %w", err)
		}

		return nil
	}

	tail := capBytes - headBytes
	cut := size - capBytes

	if _, err := io.Copy(w, io.NewSectionReader(ra, start, headBytes)); err != nil {
		return fmt.Errorf("copy section head: %w", err)
	}

	if _, err := fmt.Fprintf(w, "\n==== %d bytes cut ====\n", cut); err != nil {
		return fmt.Errorf("write cut marker: %w", err)
	}

	if _, err := io.Copy(w, io.NewSectionReader(ra, end-tail, tail)); err != nil {
		return fmt.Errorf("copy section tail: %w", err)
	}

	return nil
}
