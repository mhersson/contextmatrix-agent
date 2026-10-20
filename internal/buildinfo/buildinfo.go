// Package buildinfo reports the build a binary was made from: the version,
// commit and build time stamped by ldflags, and the merge history embedded
// from history/merges.log.
package buildinfo

import (
	"embed"
	"io/fs"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/changelog"
)

// Set only by -ldflags -X at build time: the one exception to the
// no-global-state rule.
var (
	version   string
	commit    string
	buildTime string
)

//go:embed all:history
var history embed.FS

// Info returns this binary's build info.
func Info() protocol.BuildInfo {
	return build(history)
}

func build(fsys fs.FS) protocol.BuildInfo {
	// A read error means no history was recorded (a bare go build in a fresh
	// clone); Parse turns nil into an empty, non-nil change list.
	raw, err := fs.ReadFile(fsys, "history/merges.log")
	if err != nil {
		raw = nil
	}

	return protocol.BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
		Changes:   changelog.Parse(raw, changelog.Limit),
	}
}
