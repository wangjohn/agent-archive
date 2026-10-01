package setupjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

// linkedHome is a user home reached through a symbolic link: the spelling
// setup names it by, and the real path the link resolves to.
func linkedHome(t *testing.T) (spelled, resolved string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	spelled = filepath.Join(t.TempDir(), "home")
	must(t, os.Symlink(resolved, spelled))
	return spelled, resolved
}

// priorUnits is what setup found at the unit files' paths.
type priorUnits string

const (
	unitsAbsent      priorUnits = "absent"
	unitsDefined     priorUnits = "defined"
	unitsLeftAsFound priorUnits = "left as found" // links with nothing at their end, never written
)

// Against systemd's own Definition, a rollback of a setup that found no job
// removes the job's own enable link, whichever spelling of a linked home the
// journal and the link use, and nothing else: not a recorded unit file left as
// setup found it, not a link at the job's name that is another job's, not a
// file there that is no link. A job that was defined before keeps every file.
func TestRestoreRemovesOnlyTheJobsOwnEnableLinkUnderSystemd(t *testing.T) {
	t.Parallel()
	const ref, other = "agent-archive-collector-0123456789ab", "agent-archive-collector-ba9876543210"
	type link struct {
		name   string
		make   func(link, home, resolved string)
		stales bool // the job's own, so it goes once nothing is defined
	}
	links := []link{
		{"none", func(string, string, string) {}, false},
		{"own, under the resolved home", func(link, _, resolved string) {
			must(t, os.Symlink(filepath.Join(resolved, ".config", "systemd", "user", ref+".timer"), link))
		}, true},
		{"own, under the home as spelled", func(link, home, _ string) {
			must(t, os.Symlink(filepath.Join(home, ".config", "systemd", "user", ref+".timer"), link))
		}, true},
		{"own, relative", func(link, _, _ string) { must(t, os.Symlink(filepath.Join("..", ref+".timer"), link)) }, true},
		{"another job's", func(link, home, _ string) {
			must(t, os.Symlink(filepath.Join(home, ".config", "systemd", "user", other+".timer"), link))
		}, false},
		{"a regular file", func(link, _, _ string) { must(t, os.WriteFile(link, []byte("x"), 0o600)) }, false},
	}
	for _, linkedSpelling := range []bool{true, false} {
		for _, before := range []priorUnits{unitsAbsent, unitsDefined, unitsLeftAsFound} {
			for _, l := range links {
				t.Run(fmt.Sprintf("linked spelling %v, before %s, link %s", linkedSpelling, before, l.name), func(t *testing.T) {
					t.Parallel()
					spelled, resolved := linkedHome(t)
					home := resolved
					if linkedSpelling {
						home = spelled
					}
					units := filepath.Join(home, ".config", "systemd", "user")
					service, timer := filepath.Join(units, ref+".service"), filepath.Join(units, ref+".timer")
					wants := filepath.Join(units, "timers.target.wants")
					must(t, os.MkdirAll(wants, 0o755))
					enable := filepath.Join(wants, ref+".timer")
					l.make(enable, home, resolved)
					otherLink := filepath.Join(wants, other+".timer")
					must(t, os.Symlink(filepath.Join(units, other+".timer"), otherLink))

					changes := []hooks.Change{{Path: service, After: []byte("new service"), Mode: 0o600}, {Path: timer, After: []byte("new timer"), Mode: 0o600}}
					switch before {
					case unitsDefined:
						for i := range changes {
							changes[i].Before, changes[i].Existed = []byte("old"), true
						}
						fallthrough
					case unitsAbsent:
						for _, c := range changes {
							must(t, local.WriteBytes(c.Path, c.After))
						}
					case unitsLeftAsFound:
						// setup never got to write them: each is a link with
						// nothing at its end, which reads as no file.
						for _, c := range changes {
							must(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), c.Path))
						}
					}
					journal := Journal{Changes: changes, Plist: service, Backend: "systemd", JobRef: ref}
					dataHome := t.TempDir()
					must(t, local.Write(JournalPath(dataHome), journal))
					sched := definitionOf{plistScheduler{newLaunchdSim()}, systemd.Scheduler{}}
					if err := Restore(dataHome, journal, func(string) (scheduler.Scheduler, error) { return sched, nil }); err != nil {
						t.Fatal(err)
					}

					_, err := os.Lstat(enable)
					if gone := os.IsNotExist(err); gone != (l.stales && before != unitsDefined) && l.name != "none" {
						t.Errorf("the enable link removed: %v (%v)", gone, err)
					}
					if target, err := os.Readlink(otherLink); err != nil || target != filepath.Join(units, other+".timer") {
						t.Errorf("another job's link was removed or changed (%q, %v)", target, err)
					}
					for _, c := range changes {
						info, err := os.Lstat(c.Path)
						switch before {
						case unitsAbsent:
							if !os.IsNotExist(err) {
								t.Errorf("%s is left after the rollback (%v)", c.Path, err)
							}
						case unitsDefined:
							if data, err := os.ReadFile(c.Path); err != nil || string(data) != "old" {
								t.Errorf("%s was not put back (%q, %v)", c.Path, data, err)
							}
						case unitsLeftAsFound:
							if err != nil || info.Mode()&os.ModeSymlink == 0 {
								t.Errorf("%s, as setup found it, was removed or changed (%v)", c.Path, err)
							}
						}
					}
					if TransactionPending(dataHome) {
						t.Error("the journal remains")
					}
				})
			}
		}
	}
}

// Against launchd's own Definition, under either spelling of a linked home,
// Restore never removes a plist setup found and left as it was.
func TestRestoreRemovesNothingUnderLaunchdWhateverTheHomesSpelling(t *testing.T) {
	t.Parallel()
	for _, linkedSpelling := range []bool{true, false} {
		spelled, resolved := linkedHome(t)
		home := resolved
		if linkedSpelling {
			home = spelled
		}
		agents := filepath.Join(home, "Library", "LaunchAgents")
		must(t, os.MkdirAll(agents, 0o700))
		plist := filepath.Join(agents, "com.agent-archive.collector.plist")
		must(t, os.Symlink(filepath.Join(t.TempDir(), "gone.plist"), plist))
		dataHome := t.TempDir()
		journal := Journal{Changes: []hooks.Change{{Path: plist, After: []byte("new"), Mode: 0o600}}, Plist: plist, Backend: "launchd", JobRef: "com.agent-archive.collector"}
		must(t, local.Write(JournalPath(dataHome), journal))
		sched := definitionOf{plistScheduler{newLaunchdSim()}, launchd.Scheduler{}}
		if err := Restore(dataHome, journal, func(string) (scheduler.Scheduler, error) { return sched, nil }); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(plist); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("linked spelling %v: the plist setup found was removed or changed (%v)", linkedSpelling, err)
		}
	}
}

// A stranded link that cannot be removed blocks the recovery, with the files
// already put back and the journal kept, rather than passing for a finished
// one.
func TestRestoreThatCannotRemoveTheStrandedLinkIsBlocked(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes the link from a read-only directory")
	}
	const ref = "agent-archive-collector-0123456789ab"
	units := filepath.Join(t.TempDir(), ".config", "systemd", "user")
	service, timer := filepath.Join(units, ref+".service"), filepath.Join(units, ref+".timer")
	wants := filepath.Join(units, "timers.target.wants")
	must(t, os.MkdirAll(wants, 0o755))
	link := filepath.Join(wants, ref+".timer")
	must(t, os.Symlink(timer, link))
	changes := []hooks.Change{{Path: service, After: []byte("new service"), Mode: 0o600}, {Path: timer, After: []byte("new timer"), Mode: 0o600}}
	for _, c := range changes {
		must(t, local.WriteBytes(c.Path, c.After))
	}
	must(t, os.Chmod(wants, 0o555))
	t.Cleanup(func() { _ = os.Chmod(wants, 0o755) })
	dataHome := t.TempDir()
	journal := Journal{Changes: changes, Plist: service, Backend: "systemd", JobRef: ref}
	must(t, local.Write(JournalPath(dataHome), journal))
	sched := definitionOf{plistScheduler{newLaunchdSim()}, systemd.Scheduler{}}
	err := Restore(dataHome, journal, func(string) (scheduler.Scheduler, error) { return sched, nil })
	var blocked *RecoveryBlockedError
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("err = %v, want a blocked recovery that names the removal", err)
	}
	if _, err := os.Lstat(service); !os.IsNotExist(err) {
		t.Errorf("the service unit was not rolled back first (%v)", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link is gone though removing it failed (%v)", err)
	}
	if !TransactionPending(dataHome) {
		t.Error("the journal was removed from a blocked recovery")
	}
}
