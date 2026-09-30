package cli

import (
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0), the part that calls the
// plist code directly: hooks.CollectorLabel and hooks.LaunchAgent move into the
// launchd adapter in a later PR, and this file moves with them (a mechanical
// change of the calls). TestCollectorPlistBytes pins the same bytes through
// setup; these pin them for fixed inputs, so the hash and the escaping are
// literal.

// The label of a data directory that is not the account's default one is the
// default label, a dot, and the first 12 hex digits of the SHA-256 of the
// cleaned path. The vectors are those digits as shipped: every existing
// installation's job is registered under one of them.
func TestCollectorLabelVectors(t *testing.T) {
	t.Parallel()
	const defaultHome = "/Users/alex/.local/share/agent-archive"
	for _, tc := range []struct {
		dataHome    string
		defaultHome string
		want        string
	}{
		{defaultHome, defaultHome, "com.agent-archive.collector"},
		{defaultHome + "/", defaultHome, "com.agent-archive.collector"},
		{"/Users/alex/data", defaultHome, "com.agent-archive.collector.134b03ccc4cc"},
		{"/Users/alex/data/", "", "com.agent-archive.collector.134b03ccc4cc"},
		{"/Users/alex/other/../data", "", "com.agent-archive.collector.134b03ccc4cc"},
		{"/Users/alex/data2", "", "com.agent-archive.collector.53fc22cc3ad9"},
		{defaultHome, "", "com.agent-archive.collector.3fc98f6304b1"},
	} {
		if got := hooks.CollectorLabel(tc.dataHome, tc.defaultHome); got != tc.want {
			t.Errorf("CollectorLabel(%q, %q) = %s, want %s", tc.dataHome, tc.defaultHome, got, tc.want)
		}
	}
}

// The plist for fixed inputs, byte for byte: a data directory that needs
// escaping, an environment given out of order (written sorted), and a label
// with a hash.
func TestLaunchAgentBytesForFixedInputs(t *testing.T) {
	t.Parallel()
	plist, err := hooks.LaunchAgent("/opt/agent archive/bin/agent-archive", "/Users/alex/data & <more>", "com.agent-archive.collector.134b03ccc4cc",
		map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin", "AWS_CONFIG_FILE": "/Users/alex/aws/config", "HTTPS_PROXY": "http://proxy.example:3128"})
	must(t, err)
	golden.Check(t, filepath.Join("testdata", "scheduler", "plists", "literal-fixed-inputs.plist"), plist)
	plist, err = hooks.LaunchAgent("/usr/local/bin/agent-archive", "/Users/alex/.local/share/agent-archive", hooks.LaunchLabel, nil)
	must(t, err)
	golden.Check(t, filepath.Join("testdata", "scheduler", "plists", "literal-default.plist"), plist)
}
