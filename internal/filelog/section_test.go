package filelog

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindSectionPicksTheRun(t *testing.T) {
	r1 := "==== run started T container=c1 run=R1 ====\na\n==== run ended T container=c1 run=R1 exit=1 cause=normal ====\n"
	r2 := "==== run started T container=c2 run=R2 ====\nb\nc\n==== run ended T container=c2 run=R2 exit=0 cause=normal ====\n"
	r3 := "==== run started T container=c3 run=R3 ====\nd\n"
	r10 := "==== run started T container=c4 run=R10 ====\ne\n==== run ended T container=c4 run=R10 exit=0 cause=normal ====\n"
	log := r1 + r10 + r2 + r3

	section := func(id string) (string, error) {
		start, end, err := findSection(strings.NewReader(log), id)
		if err != nil {
			return "", err
		}

		return log[start:end], nil
	}

	got, err := section("R2")
	require.NoError(t, err)
	assert.Equal(t, r2, got)

	got, err = section("R3")
	require.NoError(t, err)
	assert.Equal(t, r3, got, "a run still writing runs to the end of the file")

	got, err = section("R1")
	require.NoError(t, err)
	assert.Equal(t, r1, got, "R1 never matches the R10 header")

	_, err = section("R4")
	assert.ErrorIs(t, err, ErrNoSection)
}

func TestFindSectionTakesTheLastHeader(t *testing.T) {
	a := "==== run started T container=c1 run=R1 ====\nold\n"
	b := "==== run started T container=c1 run=R1 ====\nnew\n==== run ended T container=c1 run=R1 exit=0 cause=normal ====\n"
	log := a + b

	start, end, err := findSection(strings.NewReader(log), "R1")
	require.NoError(t, err)
	assert.Equal(t, b, log[start:end])
}

func TestFindSectionIgnoresHeadersWithoutRunID(t *testing.T) {
	log := "==== run started T container=c1 ====\nx\n"

	for _, id := range []string{"", "c1", "R1"} {
		_, _, err := findSection(strings.NewReader(log), id)
		assert.ErrorIs(t, err, ErrNoSection, id)
	}
}

func TestCopySectionKeepsSmallSectionsWhole(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 2048)
	data = append([]byte("skipped"), data...)

	var out bytes.Buffer

	require.NoError(t, copySection(&out, bytes.NewReader(data), 7, int64(len(data)), 64<<10, 8<<10))
	assert.Equal(t, data[7:], out.Bytes())
}

func TestCopySectionKeepsHeadAndTailOfALargeSection(t *testing.T) {
	data := make([]byte, 100<<10)
	for i := range data {
		data[i] = byte('a' + i%26)
	}

	var out bytes.Buffer

	require.NoError(t, copySection(&out, bytes.NewReader(data), 0, int64(len(data)), 64<<10, 8<<10))

	marker := "\n==== 36864 bytes cut ====\n"
	want := append(append(append([]byte{}, data[:8<<10]...), marker...), data[len(data)-56<<10:]...)
	assert.Equal(t, want, out.Bytes())
}

func TestCopySectionFromFile(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())
	l.Begin("p", "ABC-1", "container1234", "R1")
	l.Write("p", "ABC-1", "R1", []byte("hello"), false)
	l.End("p", "ABC-1", "R1", 0, "normal")

	var out bytes.Buffer

	require.NoError(t, l.CopySection("p", "ABC-1", "R1", &out))
	assert.Contains(t, out.String(), "hello")

	require.ErrorIs(t, l.CopySection("p", "ABC-1", "R9", &out), ErrNoSection)
	require.ErrorIs(t, l.CopySection("p", "ABC-2", "R1", &out), ErrNoSection, "no file for the card")
}
