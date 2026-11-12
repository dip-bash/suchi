// Package version holds the release version, injected at build time:
//
//	make build VERSION=0.5.0
package version

// Version is set with -ldflags "-X github.com/dip-bash/suchi/internal/version.Version=...".
var Version = "dev"

// String returns a human-readable version line.
func String() string { return "suchi " + Version }
