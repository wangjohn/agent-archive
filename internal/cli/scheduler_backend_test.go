package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// linuxInstall is one installation on a fake Linux box: the systemd adapter
// over a fake user manager, set up by the real setup command.
type linuxInstall struct {
	t        *testing.T
	env      Env
	home     string // the data directory
	userHome string
	manager  *fakeUserManager
}

// newLinuxInstall is an installation of a non-default data directory (whose
// job is agent-archive-collector-<12 hex digits>) that has not run setup.
func newLinuxInstall(t *testing.T) *linuxInstall {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	manager := newFakeUserManager(t, userHome)
	return &linuxInstall{t: t, env: manager.linuxEnv(env), home: home, userHome: userHome, manager: manager}
}

// setup runs the real setup command to completion.
func (l *linuxInstall) setup() {
	l.t.Helper()
	setupRun(l.t, l.env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, l.t.TempDir()), 0)
}

func (l *linuxInstall) ref() string { return string(l.env.installation(l.home, l.userHome).ref()) }

// units are the job's two unit files.
func (l *linuxInstall) units() (timer, service string) { return l.manager.units(l.ref()) }

// rawConfig is config.json's own keys.
func (l *linuxInstall) rawConfig() map[string]json.RawMessage {
	l.t.Helper()
	data, err := os.ReadFile(filepath.Join(l.home, "config.json"))
	must(l.t, err)
	var raw map[string]json.RawMessage
	must(l.t, json.Unmarshal(data, &raw))
	return raw
}

// asksFor replaces the real hosts's scheduler lookup for the test with one that
// records each name it is asked for, and makes the fake user manager the
// scheduler of "systemd" and of "" (this system's own), as on Linux; any other
// name is refused, as a system without its manager refuses it. The env's own
// stand-in is cleared, so a command resolves its scheduler itself. It sets
// newScheduler, so a test that calls it must not be parallel.
func (l *linuxInstall) asksFor(t *testing.T) *lookups {
	t.Helper()
	asked := &lookups{}
	previous := newScheduler
	newScheduler = func(name string) (scheduler.Scheduler, error) {
		asked.add(name)
		if name != "" && name != "systemd" {
			return nil, fmt.Errorf("no %s scheduler on this system", name)
		}
		return l.manager.scheduler(), nil
	}
	t.Cleanup(func() { newScheduler = previous })
	l.env.Scheduler = nil
	return asked
}

// lookups are the backend names the commands asked newScheduler for.
type lookups struct {
	mu    sync.Mutex
	names []string
}

func (l *lookups) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

// only fails unless every lookup so far, and at least one, was for name.
func (l *lookups) only(t *testing.T, what, name string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.names) == 0 {
		t.Errorf("%s asked for no scheduler", what)
	}
	for _, got := range l.names {
		if got != name {
			t.Errorf("%s asked for the %q scheduler, want only %q: %q", what, got, name, l.names)
			return
		}
	}
}

func (l *lookups) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = nil
}

// Setup on Linux defines the job as systemd's two unit files, loads it through
// the user manager, and records the backend it used, so a journal it writes
// says "systemd" (never launchd's name) and names the unit and its files.
func TestSetupOnLinuxDefinesTheJobForSystemdAndRecordsIt(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()

	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want a private unit file", path, info, err)
		}
	}
	if got := l.manager.held(l.ref()); got != scheduler.Loaded {
		t.Errorf("the manager holds the job as %q, want loaded", got)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + l.ref() + ".timer"}
	if got := l.manager.changing(); !slices.Equal(got, want) {
		t.Errorf("setup changed the manager by %q, want %q", got, want)
	}
	if got := string(l.rawConfig()["background_backend"]); got != `"systemd"` {
		t.Errorf(`config.json records background_backend %s, want "systemd"`, got)
	}
	if !strings.HasPrefix(l.ref(), "agent-archive-collector-") || len(l.ref()) != len("agent-archive-collector-")+12 {
		t.Errorf("the job of a non-default installation is %q", l.ref())
	}

	// The journal the same setup writes: planned from the saved configuration
	// as setup plans, it names the backend and the job, and is driven through
	// the systemd adapter by whoever recovers it.
	cfg := mustLoadConfig(t, l.home)
	next := cfg
	journal, err := planSetupTransaction(l.home, l.userHome, cfg.InstalledExecutable, cfg, &next, l.env.choosingBackend())
	must(t, err)
	if journal.Backend != "systemd" || journal.JobRef != l.ref() || journal.Plist != service {
		t.Errorf("journal backend %q, job_ref %q, definition %q; want systemd, %s, %s", journal.Backend, journal.JobRef, journal.Plist, l.ref(), service)
	}
	if !journal.WasLoaded {
		t.Errorf("the journal of a setup over a loaded job says it was not loaded")
	}
	site, ref, err := l.env.scheduler().Locate(journal.Plist)
	if err != nil || site.UserHome != filepath.Clean(l.userHome) || string(ref) != l.ref() {
		t.Errorf("Locate(%s) = %v, %q, %v", journal.Plist, site, ref, err)
	}
}

// A setup that is interrupted writes a journal that says systemd, and the next
// setup recovers it through systemd (not through a launchd it could never
// reach): the loaded job is stopped, the unit files go back as they were, and
// the job is loaded again.
func TestInterruptedLinuxSetupIsRecoveredThroughSystemd(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	cfg := mustLoadConfig(t, l.home)
	next := cfg
	journal, err := planSetupTransaction(l.home, l.userHome, cfg.InstalledExecutable, cfg, &next, l.env.choosingBackend())
	must(t, err)
	timer, service := l.units()
	before := map[string][]byte{}
	for _, path := range []string{timer, service} {
		data, err := os.ReadFile(path)
		must(t, err)
		before[path] = data
	}
	// The interruption: the job was stopped and the new unit files written.
	must(t, l.env.unloadJob(l.userHome, scheduler.Ref(l.ref())))
	for path := range before {
		must(t, os.WriteFile(path, []byte("[Unit]\nDescription=half written\n"), 0o600))
	}
	// A journal is the changes and what they were before; put the record where
	// setup leaves it while it works, so recovery finds it.
	recorded := journal
	for i := range recorded.Changes {
		if _, ok := before[recorded.Changes[i].Path]; ok {
			recorded.Changes[i].After = []byte("[Unit]\nDescription=half written\n")
		}
	}
	raw, err := json.Marshal(recorded)
	must(t, err)
	must(t, os.WriteFile(setupjournal.JournalPath(l.home), raw, 0o600))
	l.manager.calls = nil

	must(t, recoverSetup(l.home, l.env))
	if _, err := os.Stat(setupjournal.JournalPath(l.home)); !os.IsNotExist(err) {
		t.Errorf("the journal is still there after recovery: %v", err)
	}
	for path, want := range before {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s = %q, %v after recovery, want it as it was", path, got, err)
		}
	}
	if got := l.manager.held(l.ref()); got != scheduler.Loaded {
		t.Errorf("recovery left the job %q, want it loaded again", got)
	}
	for _, call := range l.manager.all() {
		if strings.Contains(call, "launchctl") {
			t.Errorf("recovery ran %q", call)
		}
	}
}

// status, uninstall, setup --refresh and a journal's recovery address the job
// through the backend config.json records, and never choose one: a nil
// Env.Scheduler means the recorded name, whatever this system would pick.
func TestCommandsAddressTheRecordedBackend(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	l.setup()
	cfg := mustLoadConfig(t, l.home)
	if cfg.BackgroundBackend != "systemd" {
		t.Fatalf("setup recorded %q", cfg.BackgroundBackend)
	}
	asked := l.asksFor(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	var status struct {
		Background string `json:"background"`
	}
	must(t, json.Unmarshal(out.Bytes(), &status))
	if status.Background != "loaded" {
		t.Errorf("status says the background job is %q, want loaded", status.Background)
	}
	asked.only(t, "status", "systemd")

	// Refresh, from a binary in a new place: the unit files are rewritten and
	// the loaded job is stopped and loaded again through the same manager.
	asked.reset()
	l.manager.calls = nil
	upgradedTo(t, &l.env)
	if code, stdout, stderr := refreshRun(t, l.env); code != 0 {
		t.Fatalf("refresh: exit %d\n%s%s", code, stdout, stderr)
	}
	asked.only(t, "setup --refresh", "systemd")
	changes := l.manager.changing()
	if len(changes) < 3 || changes[len(changes)-1] != "systemctl --user enable --now "+l.ref()+".timer" || changes[len(changes)-2] != "systemctl --user daemon-reload" {
		t.Errorf("refresh changed the manager by %q, want it to end with a reload and the job loaded again", changes)
	}

	asked.reset()
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"uninstall", "--yes"}, strings.NewReader(""), &out, &errOut, l.env); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s%s", code, &out, &errOut)
	}
	asked.only(t, "uninstall", "systemd")
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s: %v", path, err)
		}
	}
	if got := l.manager.held(l.ref()); got != scheduler.Missing {
		t.Errorf("uninstall left the job %q", got)
	}
	if kept := mustLoadConfig(t, l.home); kept.BackgroundBackend != "systemd" || kept.Archive.Enabled {
		t.Errorf("uninstall left the configuration %+v, want it disabled and still recording systemd", kept)
	}
}

// Setup picks this system's own scheduler, and records it: whatever an
// earlier configuration recorded, and with nothing to retire through a
// backend this system cannot reach. Every other command reads the record.
func TestSetupChoosesThisSystemsSchedulerAndOtherCommandsTheRecordedOne(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	asked := l.asksFor(t)
	must(t, os.MkdirAll(l.home, 0o700))
	for _, recorded := range []string{"", "systemd", "cron"} {
		cfg := config.Config{BackgroundBackend: recorded}
		must(t, config.Save(l.home, cfg))
		asked.reset()
		l.env.scheduler()
		asked.only(t, "a command addressing the job", recorded)
		asked.reset()
		got := l.env.choosingBackend().scheduler()
		asked.only(t, "setup", "")
		if got.Name() != "systemd" {
			t.Errorf("with %q recorded, setup picked %q, want this system's systemd", recorded, got.Name())
		}
	}
}

// The setup command itself chooses: over a configuration that records a
// backend this system cannot use (one copied from a Mac and hand-edited, say),
// it asks for this system's own scheduler alone, defines and loads the job
// through it, and overwrites the record.
func TestSetupOverARecordThisSystemCannotUseRecordsItsOwn(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	must(t, os.MkdirAll(l.home, 0o700))
	must(t, config.Save(l.home, config.Config{BackgroundBackend: "launchd"}))
	asked := l.asksFor(t)
	l.setup()
	// "" while it plans, and "systemd" as its journal names the job's backend.
	if names := asked.names; len(names) == 0 || slices.ContainsFunc(names, func(name string) bool { return name != "" && name != "systemd" }) {
		t.Errorf("setup asked for %q, want this system's own alone", names)
	}
	if got := mustLoadConfig(t, l.home).BackgroundBackend; got != "systemd" {
		t.Errorf("setup left background_backend %q, want systemd", got)
	}
	if got := l.manager.held(l.ref()); got != scheduler.Loaded {
		t.Errorf("the manager holds the job as %q, want loaded", got)
	}
}

// A backend that is recorded but that this system cannot use is not replaced
// by another: status reports the job unknown, and what to do; it is never
// asked about through a scheduler that was not the job's.
func TestARecordedBackendThisSystemCannotUseLeavesTheJobUnknown(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	l.setup()
	cfg := mustLoadConfig(t, l.home)
	cfg.BackgroundBackend = "cron"
	must(t, config.Save(l.home, cfg))
	asked := l.asksFor(t)
	l.manager.calls = nil
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, l.env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	var status struct {
		Background string `json:"background"`
	}
	must(t, json.Unmarshal(out.Bytes(), &status))
	if status.Background != "unknown" {
		t.Errorf("status says the background job is %q, want unknown", status.Background)
	}
	asked.only(t, "status", "cron")
	if got := l.manager.all(); len(got) != 0 {
		t.Errorf("a scheduler that was not the job's was asked: %q", got)
	}
}

// Recovery resolves the backend the journal names, by name: a journal that
// names a scheduler this system cannot use is refused before anything changes.
func TestRecoveryResolvesTheJournalsBackendByName(t *testing.T) {
	// Not parallel: asksFor replaces the scheduler lookup.
	l := newLinuxInstall(t)
	asked := l.asksFor(t)
	backends := l.env.backends()
	s, err := backends("systemd")
	if err != nil || s.Name() != "systemd" {
		t.Errorf("backends(systemd) = %v, %v", s, err)
	}
	if _, err := backends("launchd"); err == nil || !strings.Contains(err.Error(), "used the launchd scheduler, and this system's is systemd") {
		t.Errorf("backends(launchd) = %v, want a refusal that says which scheduler this system has", err)
	}
	if got := asked.names; !slices.Equal(got, []string{"systemd", "launchd", ""}) {
		t.Errorf("recovery asked for %q", got)
	}
}

// A macOS installation records nothing: its config.json has no
// background_backend key, so it is what every earlier release wrote, and an
// earlier release that rewrites it loses nothing.
func TestMacOSSetupRecordsNoBackend(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	if bytes.Contains(data, []byte("background_backend")) {
		t.Errorf("a macOS config.json records a backend:\n%s", data)
	}
	if got := mustLoadConfig(t, home).BackgroundBackend; got != "" {
		t.Errorf("BackgroundBackend = %q", got)
	}
}
