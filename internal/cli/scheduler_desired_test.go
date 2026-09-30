package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// The changes a plan makes are each file as it is now and what it becomes, as
// the setup journal records them, and only files can be written.
func TestArtifactChangesReadEachFileAsItIs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	existing, absent := filepath.Join(dir, "existing"), filepath.Join(dir, "absent")
	must(t, os.WriteFile(existing, []byte("old"), 0o600))
	changes, err := artifactChanges([]scheduler.Artifact{
		scheduler.FileArtifact(existing, []byte("new"), 0o600),
		scheduler.FileArtifact(absent, []byte("created"), 0o640),
	})
	must(t, err)
	if len(changes) != 2 {
		t.Fatalf("changes %+v", changes)
	}
	if c := changes[0]; c.Path != existing || string(c.Before) != "old" || string(c.After) != "new" || !c.Existed || c.Mode != 0o600 {
		t.Errorf("existing file: %+v", c)
	}
	if c := changes[1]; c.Path != absent || c.Before != nil || string(c.After) != "created" || c.Existed || c.Mode != 0o640 {
		t.Errorf("absent file: %+v", c)
	}
	for name, artifacts := range map[string][]scheduler.Artifact{
		"nothing to write": nil,
		"not a file":       {{ID: "crontab:me", After: []byte("x")}},
	} {
		if _, err := artifactChanges(artifacts); err == nil {
			t.Errorf("%s: artifactChanges accepted %+v", name, artifacts)
		}
	}
	// A file that cannot be read for a reason other than being absent stops the plan.
	if _, err := artifactChanges([]scheduler.Artifact{scheduler.FileArtifact(dir, []byte("x"), 0o600)}); err == nil {
		t.Error("artifactChanges accepted a directory")
	}
}

// The collector's job is `_collect` every minute and once at load, with the
// environment its storage needs and no other setting.
func TestCollectorJobIsTheCollectorEveryMinute(t *testing.T) {
	t.Parallel()
	got := collectorJob("/bin/agent-archive", "/data", map[string]string{"PATH": "/usr/bin"})
	if got.Executable != "/bin/agent-archive" || strings.Join(got.Args, " ") != "_collect" || got.DataHome != "/data" || got.Env["PATH"] != "/usr/bin" || got.Interval != time.Minute || !got.RunAtLoad {
		t.Errorf("collectorJob = %+v", got)
	}
}

// twoFileDefiner is a scheduler that keeps a job in two files, as systemd
// keeps a .service and a .timer.
type twoFileDefiner struct{ launchd.Scheduler }

func (twoFileDefiner) Plan(site scheduler.Site, _ scheduler.Installation, spec scheduler.JobSpec) (scheduler.Plan, error) {
	dir := filepath.Join(site.UserHome, "units")
	return scheduler.Plan{Ref: "job", Artifacts: []scheduler.Artifact{
		scheduler.FileArtifact(filepath.Join(dir, "job.service"), []byte("ExecStart="+spec.Executable), 0o600),
		scheduler.FileArtifact(filepath.Join(dir, "job.timer"), []byte("OnUnitActiveSec=60"), 0o600),
	}}, nil
}

// Refresh redefines a job from its definition on disk: every file the
// scheduler keeps it in, nothing when it already runs the executable (even
// when the rest of it cannot be read), and the definition's error otherwise.
func TestRefreshJobRedefinesEveryFileOrNothing(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	in := installation{home: "/data", userHome: userHome, accountHome: userHome, sched: func() scheduler.Scheduler { return twoFileDefiner{} }}
	defined := func(program string, err error) scheduler.Status {
		return scheduler.Status{Defined: true, Program: program, Env: map[string]string{}, Paths: []string{"/defs/job"}, DefinitionErr: err}
	}
	changes, err := refreshJob(in, userHome, "/data", "/new/agent-archive", defined("/old/agent-archive", nil))
	must(t, err)
	if len(changes) != 2 || filepath.Base(changes[0].Path) != "job.service" || filepath.Base(changes[1].Path) != "job.timer" || string(changes[0].After) != "ExecStart=/new/agent-archive" {
		t.Errorf("refresh of a job in two files changed %+v", changes)
	}
	// A definition that already runs exe is left alone, even when its
	// environment cannot be read.
	envErr := errors.New("environment cut off")
	if changes, err := refreshJob(in, userHome, "/data", "/new/agent-archive", defined("/new/agent-archive", envErr)); err != nil || len(changes) != 0 {
		t.Errorf("a definition running exe: %+v, %v", changes, err)
	}
	if _, err := refreshJob(in, userHome, "/data", "/new/agent-archive", defined("/old/agent-archive", envErr)); err == nil || err.Error() != "/defs/job cannot be read (environment cut off); run agent-archive setup to write it again" {
		t.Errorf("a definition whose environment cannot be read: %v", err)
	}
	if changes, err := refreshJob(in, userHome, "/data", "/new/agent-archive", scheduler.Status{Paths: []string{"/defs/job"}}); err != nil || len(changes) != 0 {
		t.Errorf("no definition: %+v, %v", changes, err)
	}
}

// status checks the environment a collector's definition sets against the
// PATH its scheduler gives a job that sets none, and says nothing about an
// environment it cannot read.
func TestStatusChecksTheCollectorsEnvironmentAsTheSchedulerRunsIt(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	userHome, err := env.UserHomeDir()
	must(t, err)
	configFile, _, _ := awsFixture(t)
	exe := filepath.Join(t.TempDir(), "agent-archive")
	must(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755))
	in := env.installation(home, userHome)
	plist := in.collectorPlist()
	must(t, os.MkdirAll(filepath.Dir(plist), 0o700))
	cfg := config.Config{Archive: archive.Config{Enabled: true}, Storage: credentials.Config{Provider: credentials.ProviderS3, AWSProfile: "vault"}}

	// No PATH in the plist: the collector runs with launchd's.
	data, err := launchd.LaunchAgent(exe, home, in.label(), map[string]string{"AWS_CONFIG_FILE": configFile})
	must(t, err)
	must(t, os.WriteFile(plist, data, 0o600))
	background := readBackground(&statusView{}, cfg, home, userHome, env)
	want := `AWS profile "vault" gets its credentials by running vault-helper, which the background collector cannot find on its PATH (/usr/bin:/bin:/usr/sbin:/sbin).`
	if !slices.Equal(background.environmentProblems, []string{want}) {
		t.Errorf("environment problems %q, want %q", background.environmentProblems, want)
	}

	// A plist whose environment cannot be read is not checked as though it
	// set none, which would read the profile from ~/.aws.
	must(t, os.MkdirAll(filepath.Join(userHome, ".aws"), 0o700))
	profiles, err := os.ReadFile(configFile)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(userHome, ".aws", "config"), profiles, 0o600))
	must(t, os.WriteFile(plist, []byte(`<plist><dict><key>ProgramArguments</key><array><string>`+exe+`</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>/bin</string></key>`), 0o600))
	view := statusView{}
	background = readBackground(&view, cfg, home, userHome, env)
	if len(background.environmentProblems) != 0 || len(view.Warnings) != 0 || background.program != exe {
		t.Errorf("an unreadable environment: problems %q, warnings %q, program %q", background.environmentProblems, view.Warnings, background.program)
	}
}

// A status a scheduler gives with no problem (a fake that forgot it) is an
// empty one, not a nil dereference.
func TestProblemOfAStatusWithNone(t *testing.T) {
	t.Parallel()
	if got := problemOf(scheduler.Status{State: scheduler.Unknown}); got != (scheduler.Problem{}) {
		t.Errorf("problemOf = %+v", got)
	}
	want := scheduler.Problem{Kind: scheduler.ProblemNotOwned, Ref: "x", Expected: "/e"}
	if got := problemOf(scheduler.Status{Problem: &want}); got != want {
		t.Errorf("problemOf = %+v, want %+v", got, want)
	}
}
