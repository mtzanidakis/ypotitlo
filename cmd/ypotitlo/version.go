package main

import (
	"runtime/debug"
	"strings"
)

// resolveVersion returns the version to report, preferring the linker-stamped
// value and falling back to the module version recorded in the binary.
//
// Both sources are needed because the two ways of installing this tool populate
// different ones. A release build passes -ldflags -X main.version, so `version`
// is set. `go install ...@v0.1.0` passes no ldflags at all, so `version` keeps
// its "dev" default even though the binary was built from a tagged release —
// which is exactly the confusing case this exists to fix. The Go toolchain does
// record the module version in the build info for such builds, so that is where
// the answer comes from.
//
// A local `go build` has neither, and honestly reports dev.
func resolveVersion() string {
	if v := normalizeVersion(version); v != "" && v != devVersion {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		// "(devel)" is what the toolchain records for a build from a working
		// tree rather than a tagged module, which is no better than "dev".
		if v := normalizeVersion(bi.Main.Version); v != "" && v != "(devel)" {
			return v
		}
	}
	return devVersion
}

const devVersion = "dev"

// normalizeVersion trims whitespace and ensures a leading "v" on anything that
// looks like a version number, so that a binary stamped "0.1.0" by goreleaser
// and one reporting "v0.1.0" from build info print the same string. Tags in this
// repo carry the prefix; the goreleaser asset names do not.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == devVersion {
		return v
	}
	if v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}
