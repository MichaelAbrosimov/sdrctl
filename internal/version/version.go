// Package version holds the build version of sdrctl.
package version

// Version is overridden at build time via:
//
//	-ldflags "-X github.com/MichaelAbrosimov/sdrctl/internal/version.Version=<v>"
var Version = "0.1.0-dev"
