package backfill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A lookup can return anything; only a repository key may reach a
// registration file.
func TestRegistrationNeverRecordsAnythingButARepoKey(t *testing.T) {
	t.Parallel()
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
	path := filepath.Join(project, "a.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Harness: "claude", NativeSessionID: "a", TranscriptPath: path, ProjectRoot: project, StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}
	r := Registration{
		Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted,
		RepoKey: func(string) string { return "https://user:synthetic-token@example.test/acme/widget.git" },
	}
	result, err := r.Run([]Candidate{candidate})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "registrations", result.Sessions[0]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-token") || strings.Contains(string(raw), "example.test") || strings.Contains(string(raw), "repo_key") {
		t.Errorf("a registration file carries something that is not a repo key: %s", raw)
	}
}

// Git is not run for a session that will not be registered: an excluded
// project, or one not yet activated.
func TestRegistrationOnlyAsksAboutSessionsTheConfigurationAdmits(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	excluded, later := t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
		{ProjectID: archive.ProjectID(excluded), Root: excluded, Included: false},
		{ProjectID: archive.ProjectID(later), Root: later, Included: true, ActivatedAt: admitted.Add(time.Hour)},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidate := func(id, root string) Candidate {
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Candidate{Harness: "claude", NativeSessionID: id, TranscriptPath: path, ProjectRoot: root, StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}
	}
	var asked []string
	r := Registration{
		Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted,
		RepoKey: func(root string) string { asked = append(asked, root); return "" },
	}
	result, err := r.Run([]Candidate{candidate("in", project), candidate("out", excluded), candidate("early", later)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 || len(asked) != 1 || asked[0] != project {
		t.Errorf("registered %d sessions and asked about %v, want one session and only %s", len(result.Sessions), asked, project)
	}
}
