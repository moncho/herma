// Package version holds the version stamped into the binary at build time.
package version

// Version is set by the linker from the git tag (see the Makefile); a plain
// go build leaves it as dev.
var Version = "dev"
