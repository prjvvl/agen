// Package version holds the platform build version.
package version

// Version is overridden at build time with -ldflags "-X ...version.Version=...".
var Version = "dev"

// String returns the human-readable version line.
func String(bin string) string { return bin + " " + Version }
