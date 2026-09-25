package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Regression: phase-1 review follow-up. The default installation and each
// installation's launchd label compared data directory paths as exact
// strings, while hooks tell installations apart by location: another case
// of the default directory's name (one directory on macOS's default
// volumes) got a label of its own, so one data directory could end up with
// two background jobs. A symlinked spelling already worked; both do now,
// for the default directory and any other.
func TestEverySpellingOfADataDirectoryIsOneInstallation(t *testing.T) {
	account, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := Env{AccountHome: func() (string, error) { return account, nil }}
	accountDefault := filepath.Join(account, ".local", "share", "agent-archive")
	other := filepath.Join(account, "Test-Install")
	for _, dir := range []string{accountDefault, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(account, "link-to-share")
	if err := os.Symlink(filepath.Join(account, ".local", "share"), link); err != nil {
		t.Fatal(err)
	}
	spellings := map[string][]string{
		accountDefault: {accountDefault, accountDefault + "/", filepath.Join(link, "agent-archive")},
		other:          {other, other + "/", filepath.Join(account, ".", "Test-Install")},
	}
	if caseInsensitiveVolume(t, account) {
		spellings[accountDefault] = append(spellings[accountDefault], filepath.Join(account, ".local", "share", "Agent-Archive"), filepath.Join(account, ".LOCAL", "Share", "AGENT-ARCHIVE"))
		spellings[other] = append(spellings[other], filepath.Join(account, "test-install"), strings.ToUpper(other))
	} else {
		t.Log("case-sensitive volume: the other-case spellings are skipped")
	}
	for dir, all := range spellings {
		want := env.installation(dir, account)
		for _, spelling := range all {
			in := env.installation(spelling, account)
			if in.isDefault() != (dir == accountDefault) {
				t.Errorf("%s: isDefault=%v", spelling, in.isDefault())
			}
			if in.label() != want.label() {
				t.Errorf("%s: label %s, want %s", spelling, in.label(), want.label())
			}
			if (in.label() == hooks.LaunchLabel) != (dir == accountDefault) {
				t.Errorf("%s: label %s", spelling, in.label())
			}
		}
	}
}

func caseInsensitiveVolume(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// An installation created before CanonicalPath keeps its launchd label when
// its data directory is spelled the way the volume lists it (as it is for
// every default installation and nearly every other one): the label is the
// one earlier releases derived, from the directory with its symlinks
// resolved. A changed label would leave the old job running beside a new
// one.
func TestExistingInstallationsKeepTheirLabel(t *testing.T) {
	account, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := Env{AccountHome: func() (string, error) { return account, nil }}
	accountDefault := filepath.Join(account, ".local", "share", "agent-archive")
	other := filepath.Join(account, "Test-Install")
	for _, dir := range []string{accountDefault, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(account, "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	// What releases before CanonicalPath computed.
	previous := func(home string) string {
		if resolved, err := local.ResolveExistingSymlinks(home); err == nil {
			home = resolved
		}
		if home == accountDefault {
			return hooks.LaunchLabel
		}
		return hooks.CollectorLabel(home, "")
	}
	for _, home := range []string{accountDefault, other, link, filepath.Join(account, "not-created-yet")} {
		if got, want := env.installation(home, account).label(), previous(home); got != want {
			t.Errorf("%s: label %s, earlier releases gave %s", home, got, want)
		}
	}
}

// Regression: PR #57 review. A data directory spelled in another case than
// its volume lists it had a label of its own, derived from that spelling;
// CanonicalPath gives it the listed spelling's label now. Setup retires the
// job under the old label, as it retires one under the default label, so
// one directory never has two background jobs.
func TestSetupRetiresTheJobOfAnotherCaseSpelling(t *testing.T) {
	base := t.TempDir()
	if !caseInsensitiveVolume(t, base) {
		t.Skip("case-sensitive volume: another case is another directory")
	}
	if err := os.Mkdir(filepath.Join(base, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	home, userHome := filepath.Join(base, "DATA"), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	resolved, err := local.ResolveExistingSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	oldLabel := hooks.CollectorLabel(resolved, "")
	if oldLabel == env.installation(home, userHome).label() {
		t.Fatal("the other-case spelling kept its own label; the test no longer covers the migration")
	}
	old := filepath.Join(userHome, "Library", "LaunchAgents", oldLabel+".plist")
	plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", home, oldLabel)
	if err := local.WriteBytes(old, plist); err != nil {
		t.Fatal(err)
	}
	if got := env.installation(home, userHome).previousCollectorPlists(); len(got) != 1 || got[0] != old {
		t.Fatalf("previousCollectorPlists = %q, want %q", got, old)
	}
	states := map[string]string{old: "loaded"}
	var unloaded []string
	env.JobState = func(p string) string { return states[p] }
	env.LoadLaunchAgent = func(p string) error { states[p] = "loaded"; return nil }
	env.UnloadLaunchAgent = func(p string) error { unloaded = append(unloaded, p); states[p] = "missing"; return nil }
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	if _, err := os.Stat(old); !os.IsNotExist(err) || len(unloaded) != 1 || unloaded[0] != old {
		t.Fatalf("the other spelling's job was not retired: unloaded %v, stat %v", unloaded, err)
	}
	if states[env.installation(home, userHome).collectorPlist()] != "loaded" {
		t.Fatal("the collector was not loaded under the directory's label")
	}
}
