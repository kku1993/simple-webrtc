// Package version holds the build-time version of the signaling server.
//
// Version is stamped at build time via -ldflags "-X ...version.Version=...".
// When the server is built with the repo's scripts/build.sh helper, the value
// is read from the repo-root VERSION file (a single line of the form
// "major.minor.patch"). When the variable is not overridden at link time,
// Version falls back to "dev" so that a plain `go build` still produces a
// runnable binary.
package version

// Version is the server version string. It is overridden at link time via
// -ldflags "-X github.com/kku1993/simple-webrtc-server/internal/version.Version=<v>".
var Version = "dev"
