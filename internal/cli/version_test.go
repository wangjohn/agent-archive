package cli

import (
	"runtime/debug"
	"testing"
)

// A source build reports the commit it was built from, so a bug report says
// which code ran; a released build reports its version unchanged.
func TestDescribeVersionNamesTheCommitOfADevBuild(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	vcs := func(settings ...string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		for i := 0; i+1 < len(settings); i += 2 {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return info
	}
	cases := []struct {
		name    string
		version string
		info    *debug.BuildInfo
		want    string
	}{
		{"release", "v0.1.0", vcs("vcs.revision", sha), "v0.1.0"},
		{"dev with commit", "dev", vcs("vcs.revision", sha, "vcs.modified", "false"), "dev-0123456789ab"},
		{"dev with uncommitted changes", "dev", vcs("vcs.revision", sha, "vcs.modified", "true"), "dev-0123456789ab-dirty"},
		{"dev with a short revision", "dev", vcs("vcs.revision", "abc"), "dev-abc"},
		{"dev without VCS information", "dev", vcs("-trimpath", "true"), "dev"},
		{"dev without build information", "dev", nil, "dev"},
	}
	for _, c := range cases {
		if got := describeVersion(c.version, c.info); got != c.want {
			t.Errorf("%s: describeVersion = %q, want %q", c.name, got, c.want)
		}
	}
}
