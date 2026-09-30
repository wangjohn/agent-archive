package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
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
