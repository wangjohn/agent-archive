package backfill

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// registerImported imports one candidate per id from a single project and
// returns each registration, asking lookup (which may be nil) for the
// project's repository key.
func registerImported(t *testing.T, lookup func(root string) string, ids ...string) []archive.SessionRegistration {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	for _, id := range ids {
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, Candidate{Harness: "claude", NativeSessionID: id, TranscriptPath: path, ProjectRoot: project, StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript})
	}
	// One step per hold, so the sessions register across several holds.
	r := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted, MaxHoldSteps: 1, RepoKey: lookup}
	result, err := r.Run(candidates)
	if err != nil || len(result.Sessions) != len(ids) {
		t.Fatalf("%+v %v", result, err)
	}
	var regs []archive.SessionRegistration
	for _, id := range result.Sessions {
		reg, found, err := store.LoadRegistration(id)
		if err != nil || !found {
			t.Fatalf("registration %s: found=%t err=%v", id, found, err)
		}
		regs = append(regs, reg)
	}
	return regs
}

func TestRegistrationRecordsTheRepoKeyAskingOncePerProject(t *testing.T) {
	t.Parallel()
	want := archive.RepoKey("https://example.test/acme/widget.git")
	asked := 0
	regs := registerImported(t, func(string) string { asked++; return want }, "a", "b", "c")
	for _, reg := range regs {
		if reg.RepoKey != want {
			t.Errorf("%s: RepoKey = %q, want %q", reg.NativeSessionID, reg.RepoKey, want)
		}
	}
	if asked != 1 {
		t.Errorf("the lookup ran %d times for one project, want 1", asked)
	}
}

func TestRegistrationWithoutARepoKeyRegistersWithoutOne(t *testing.T) {
	t.Parallel()
	for name, lookup := range map[string]func(string) string{
		"no lookup":              nil,
		"a project with no repo": func(string) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, reg := range registerImported(t, lookup, "a") {
				if reg.RepoKey != "" {
					t.Errorf("RepoKey = %q, want none", reg.RepoKey)
				}
			}
		})
	}
}
