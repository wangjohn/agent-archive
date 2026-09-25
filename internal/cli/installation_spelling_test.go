package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
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
