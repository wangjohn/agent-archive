package setupjournal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

const legacyPlist = `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.agent-skills.skill-runs-upload</string><key>ProgramArguments</key><array><string>/usr/bin/python3</string><string>/private/runtime/skill_runs.py</string><string>--home</string><string>/private/records</string><string>upload</string></array></dict></plist>`

func TestLegacyMigrationRejectsUnownedJob(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "Library", "LaunchAgents", LegacyLaunchLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(legacyPlist, "skill_runs.py", "unrelated.py", 1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanLegacyMigration(home, fakeLaunchd{state: func(string) string { return "loaded" }}); err == nil {
		t.Fatal("accepted unowned job")
	}
	after, _ := os.ReadFile(path)
	if string(after) != data {
		t.Fatal("unowned job changed")
	}
}

// Without a legacy plist, the legacy migration needs no answer from
// launchctl. (Setup itself still does, for its own job's label: see
// TestFirstSetupRefusesAnUnknownJobState.)
func TestLegacyMigrationWithoutALegacyJobNeedsNoLaunchctl(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	launchd := fakeLaunchd{state: func(string) string { return "unknown" }}
	if job, err := PlanLegacyMigration(userHome, launchd); err != nil || job != nil {
		t.Fatalf("no legacy plist must not need launchctl: job=%+v err=%v", job, err)
	}
}

// A crash after the legacy job was retired and the new collector's files
// were written, but before the collector was started, leaves a journal with
// a Legacy entry. Recovery must restore and reload the prototype job and
// remove the half-installed collector.
func TestRecoverSetupReplaysLegacyJournal(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	legacyPath := filepath.Join(userHome, "Library", "LaunchAgents", LegacyLaunchLabel+".plist")
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	states := map[string]string{}
	var loaded []string
	launchd := fakeLaunchd{
		state: func(p string) string {
			if s := states[p]; s != "" {
				return s
			}
			return "missing"
		},
		load:   func(p string) error { states[p] = "loaded"; loaded = append(loaded, p); return nil },
		unload: func(p string) error { states[p] = "missing"; return nil },
	}
	// Simulated crash state: legacy plist already removed and unloaded, the
	// new plist and config written but the collector never started.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("new plist"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(configPath, []byte("new config"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := Journal{
		Legacy:  &LegacyJob{Change: hooks.Change{Path: legacyPath, Before: []byte(legacyPlist), Existed: true, Mode: 0600}, WasLoaded: true},
		Changes: []hooks.Change{{Path: plistPath, After: []byte("new plist"), Mode: 0600}, {Path: configPath, After: []byte("new config"), Mode: 0600}},
		Plist:   plistPath,
	}
	if err := local.Write(JournalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	if err := Recover(home, launchd, noLock); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(legacyPath); err != nil || string(data) != legacyPlist {
		t.Fatalf("legacy job not restored: %q %v", data, err)
	}
	if states[legacyPath] != "loaded" || len(loaded) != 1 || loaded[0] != legacyPath {
		t.Fatalf("legacy job not reloaded (and nothing else started): states=%v loaded=%v", states, loaded)
	}
	for _, p := range []string{plistPath, configPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
	if TransactionPending(home) {
		t.Fatal("journal not removed")
	}
	// Recovery is idempotent once the journal is gone.
	if err := Recover(home, launchd, noLock); err != nil {
		t.Fatal(err)
	}
}
