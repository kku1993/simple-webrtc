// Package version holds the build-time version of the signaling server.
//
// Version is stamped at build time via -ldflags "-X ...version.Version=...".
// When the server is built with the repo's scripts/build.sh helper, the value
// is read from the repo-root VERSION file (a single line of the form
// "major.minor.patch"). When the variable is not overridden at link time,
// Version falls back to "dev" so that a plain `go build` still produces a
// runnable binary.
package version

import (
	"strconv"
	"strings"
)

// Version is the server version string. It is overridden at link time via
// -ldflags "-X github.com/kku1993/simple-webrtc-server/internal/version.Version=<v>".
var Version = "dev"

// Major returns the major version number parsed from Version, or -1 if Version
// is "dev" or cannot be parsed. When -1, callers should skip version checks
// (development mode — a plain `go build` or `go test` without ldflags).
func Major() int {
	return MajorFromString(Version)
}

// MajorFromString parses the major version number from a semver-ish string
// ("major.minor.patch"). Returns -1 if the string is "dev", empty, or does not
// begin with a numeric major component.
func MajorFromString(s string) int {
	if s == "dev" || s == "" {
		return -1
	}
	parts := strings.SplitN(s, ".", 2)
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1
	}
	return major
}
