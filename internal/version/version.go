// Package version holds prbot's release version.
//
// Version is the single source of truth: the release workflow reads it back by
// running the built binary with -version, so bump it here and nowhere else.
package version

import "runtime/debug"

// Version is the release version, following semantic versioning.
const Version = "0.1.0"

// String returns the version with the commit it was built from, when the Go
// toolchain recorded one (it does for builds from a git checkout), for example
// "0.1.0 (commit 1a2b3c4d5e6f)" or "0.1.0 (commit 1a2b3c4d5e6f, modified)".
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return withVCS(Version, info.Settings)
}

func withVCS(v string, settings []debug.BuildSetting) string {
	var rev string
	var modified bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return v
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		return v + " (commit " + rev + ", modified)"
	}
	return v + " (commit " + rev + ")"
}

// UserAgent is the User-Agent prbot sends to GitHub, which asks API clients
// to identify themselves.
func UserAgent() string { return "llm-pr-bot/" + Version }
