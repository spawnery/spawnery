// Package version carries the operator build version.
package version

// Version is the operator version. Release builds override it via
// -ldflags "-X github.com/spawnery/spawnery/internal/version.Version=v1.2.3".
var Version = "dev"
