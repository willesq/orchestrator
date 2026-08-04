// Package version carries build-stamped identifiers for the orchestrator binaries.
package version

// These are overridable at build time via -ldflags.
var (
	// Version is the semantic version of the build.
	Version = "0.0.0-dev"
	// Commit is the git commit the build was produced from.
	Commit = "none"
)
