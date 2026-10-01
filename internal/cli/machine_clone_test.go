package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

const (
	thisMachine  = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	otherMachine = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// onMachine is the install's Linux box with the machine ID whose fingerprint
// is fingerprint ("" for one with no machine ID).
func (l *linuxInstall) onMachine(fingerprint string) {
	l.env.HostFingerprint = func() string { return fingerprint }
}

// A Linux setup records the machine it ran on beside the machine ID, once; a
// machine with no machine ID records nothing; and macOS never does, so a Mac's
// config.json is what it was before the field.
func TestSetupRecordsTheMachineItRanOnOnlyOnLinux(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.onMachine(thisMachine)
	l.setup()
	if got := mustLoadConfig(t, l.home).HostID; got != thisMachine {
		t.Errorf("setup recorded host_id %q, want %q", got, thisMachine)
	}
	if got := string(l.rawConfig()["host_id"]); got != `"`+thisMachine+`"` {
		t.Errorf("config.json has host_id %s", got)
	}

	none := newLinuxInstall(t)
	none.onMachine("")
	none.setup()
	if _, ok := none.rawConfig()["host_id"]; ok {
		t.Error("a machine with no machine ID recorded a host_id")
	}

	home, userHome := t.TempDir(), t.TempDir()
	mac := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	mac.HostFingerprint = func() string { return thisMachine }
	setupRun(t, mac, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	if got := mustLoadConfig(t, home).HostID; got != "" {
		t.Errorf("a Mac recorded host_id %q", got)
	}
}

// The recorded machine is kept by a later setup on the same machine, and by a
// later setup on a copy: running setup again does not make the copy look like
// the original.
func TestSetupKeepsTheRecordedMachine(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.onMachine(thisMachine)
	l.setup()
	l.onMachine(otherMachine)
	output := setupYes(t, l.env, "", 0, "--yes")
	if got := mustLoadConfig(t, l.home).HostID; got != thisMachine {
		t.Errorf("setup on a copy changed host_id to %q, want it kept as %q", got, thisMachine)
	}
	if !strings.Contains(output, "set up on a different machine") {
		t.Errorf("setup --yes on a copy did not say so:\n%s", output)
	}
}

// A configuration with a machine ID and no recorded machine (one from before
// the field, or from a machine that had no ID) records this one at the next
// setup, and says nothing about it.
func TestSetupRecordsTheMachineWhenNoneWasRecorded(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.onMachine("")
	l.setup()
	cfg := mustLoadConfig(t, l.home)
	cfg.HostID = ""
	must(t, config.Save(l.home, cfg))
	l.onMachine(thisMachine)
	if output := setupYes(t, l.env, "", 0, "--yes"); strings.Contains(output, "different machine") {
		t.Errorf("setup warned about a machine it had no record of:\n%s", output)
	}
	if got := mustLoadConfig(t, l.home).HostID; got != thisMachine {
		t.Errorf("host_id %q, want %q", got, thisMachine)
	}
}

// Interactive setup, too, says so before anything is applied.
func TestInteractiveSetupOnACopyWarns(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.onMachine(thisMachine)
	l.setup()
	l.onMachine(otherMachine)
	output := setupRun(t, l.env, strings.Repeat("\n", 40), 0)
	if !strings.Contains(output, "set up on a different machine") || !strings.Contains(output, "agent-archive uninstall --delete-local-data") {
		t.Errorf("setup on a copy did not warn, or did not say what to do:\n%s", output)
	}
}

// Status says the data directory was copied from another machine, with the
// fix, only when both machines are known and differ.
func TestStatusWarnsWhenTheDataDirectoryWasSetUpOnAnotherMachine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		recorded string
		now      string
		warns    bool
	}{
		{"the same machine", thisMachine, thisMachine, false},
		{"another machine", thisMachine, otherMachine, true},
		{"this machine has no ID", thisMachine, "", false},
		{"nothing was recorded", "", otherMachine, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newLinuxInstall(t)
			l.onMachine(tc.recorded)
			l.setup()
			l.onMachine(tc.now)
			var clone []string
			for _, w := range statusWarnings(t, l.env) {
				if strings.Contains(w, "different machine") {
					clone = append(clone, w)
				}
			}
			if tc.warns != (len(clone) == 1) {
				t.Fatalf("clone warnings %q, want one: %v", clone, tc.warns)
			}
			if tc.warns {
				for _, want := range []string{"agent-archive uninstall --delete-local-data", "agent-archive setup", "host_id", l.home} {
					if !strings.Contains(clone[0], want) {
						t.Errorf("warning %q does not mention %q", clone[0], want)
					}
				}
			}
		})
	}
}

// macOS never warns, whatever the field holds (the Migration Assistant case
// is the guide's), and the machine it looks at is only the Linux one.
func TestMacOSIgnoresTheRecordedMachine(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	cfg := mustLoadConfig(t, home)
	cfg.HostID = thisMachine
	must(t, config.Save(home, cfg))
	env.HostFingerprint = func() string { return otherMachine }
	for _, w := range statusWarnings(t, env) {
		if strings.Contains(w, "different machine") {
			t.Errorf("status on macOS warned: %q", w)
		}
	}
}

// The field is local: nothing published carries it, and config.json is the one
// place it is in.
func TestHostIDIsInTheConfigurationOnly(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.onMachine(thisMachine)
	l.setup()
	encoded, err := json.Marshal(mustLoadConfig(t, l.home).Archive)
	must(t, err)
	if strings.Contains(string(encoded), thisMachine) {
		t.Errorf("the archive configuration carries the host ID: %s", encoded)
	}
}
