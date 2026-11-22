package version

import (
	"regexp"
	"runtime/debug"
	"testing"
)

func TestVersionIsSemver(t *testing.T) {
	// Packaging reads this value back for deb, rpm and Arch package versions,
	// none of which accept a leading "v" or a "-" inside the version.
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("Version %q is not MAJOR.MINOR.PATCH", Version)
	}
}

func TestWithVCS(t *testing.T) {
	rev := "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{"no vcs info", nil, "1.2.3"},
		{"clean", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}, "1.2.3 (commit 0123456789ab)"},
		{"modified", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, "1.2.3 (commit 0123456789ab, modified)"},
		{"short revision", []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}, "1.2.3 (commit abc)"},
	}
	for _, c := range cases {
		if got := withVCS("1.2.3", c.settings); got != c.want {
			t.Errorf("%s: withVCS = %q, want %q", c.name, got, c.want)
		}
	}
}
