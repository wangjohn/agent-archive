package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Enabling the job left a link in timers.target.wants, and uninstall
// --skip-scheduler, which deletes the unit files without asking the manager to
// disable them, removes it too: nothing dangles for the manager to report.
func TestLinuxUninstallSkippingTheSchedulerLeavesNoDanglingEnableLink(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	link := l.manager.enableLink(l.ref())
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("setup left no enable link: %v", err)
	}
	l.manager.noBus = true
	if code, output := l.uninstall("--skip-scheduler"); code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the enable link is left after uninstall --skip-scheduler (%v)", err)
	}
}

// An ordinary uninstall leaves none either (the manager's disable removes it).
func TestLinuxUninstallLeavesNoEnableLink(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	if code, output := l.uninstall(); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, output)
	}
	if _, err := os.Lstat(l.manager.enableLink(l.ref())); !os.IsNotExist(err) {
		t.Errorf("the enable link is left after uninstall (%v)", err)
	}
}

// What uninstall removes of the job's files is what is the job's own: a link at
// the job's name that points at another installation's timer stays, and so
// does the other installation's own link.
func TestLinuxUninstallNeverRemovesAnotherInstallationsEnableLink(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	link := l.manager.enableLink(l.ref())
	other := l.manager.enableLink("agent-archive-collector-0123456789ab")
	elsewhere := filepath.Join(t.TempDir(), "agent-archive-collector-0123456789ab.timer")
	must(t, os.Remove(link))
	must(t, os.Symlink(elsewhere, link))
	must(t, os.Symlink(elsewhere, other))
	l.manager.noBus = true
	if code, output := l.uninstall("--skip-scheduler"); code != 0 {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	for _, kept := range []string{link, other} {
		if target, err := os.Readlink(kept); err != nil || target != elsewhere {
			t.Errorf("%s was removed or changed by uninstall (%q, %v)", kept, target, err)
		}
	}
}
