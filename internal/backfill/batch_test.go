package backfill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// Batches are numbered per local day. An interrupted batch is continued by
// a run with the same filters and destination, and by no other.
func TestOpenBatch(t *testing.T) {
	home := t.TempDir()
	filters := NewBatchFilters(Filters{Harnesses: []string{"claude-code"}, Projects: []string{"/work/repo"}, IncludeTemp: true})
	if filters.Harnesses[0] != "claude" || filters.ProjectIDs[0] != archive.ProjectID("/work/repo") {
		t.Fatalf("filters %+v", filters)
	}
	b, err := OpenBatch(home, filters, "dest", fixedNow)
	if err != nil || b.ID != "2026-09-23-1" {
		t.Fatalf("%+v, %v", b, err)
	}
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	if again, _ := OpenBatch(home, filters, "dest", fixedNow.Add(time.Minute)); again.ID != b.ID {
		t.Fatalf("interrupted batch not continued: %s", again.ID)
	}
	for _, other := range []struct {
		filters BatchFilters
		dest    string
	}{{NewBatchFilters(Filters{}), "dest"}, {filters, "elsewhere"}} {
		if next, _ := OpenBatch(home, other.filters, other.dest, fixedNow); next.ID != "2026-09-23-2" {
			t.Fatalf("different run continued the batch: %s", next.ID)
		}
	}
	done := fixedNow.UTC()
	b.CompletedAt = &done
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	if next, _ := OpenBatch(home, filters, "dest", fixedNow); next.ID != "2026-09-23-2" {
		t.Fatalf("completed batch continued: %s", next.ID)
	}
	if tomorrow, _ := OpenBatch(home, filters, "dest", fixedNow.Add(24*time.Hour)); tomorrow.ID != "2026-09-24-1" {
		t.Fatalf("next day: %s", tomorrow.ID)
	}
	batches, err := LoadBatches(home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil {
		t.Fatalf("%+v, %v", batches, err)
	}
}

// The destination ID covers where sessions go, never credentials.
func TestDestinationID(t *testing.T) {
	s3 := credentials.Config{Provider: "s3", Bucket: "b", Prefix: "agent-archive/", Region: "us-east-1", AWSProfile: "p"}
	other := s3
	other.AWSProfile, other.Region, other.Prefix = "q", "eu-west-1", "/agent-archive"
	if DestinationID(s3) != DestinationID(other) {
		t.Fatal("credentials or prefix spelling changed the destination")
	}
	other.Bucket = "c"
	if DestinationID(s3) == DestinationID(other) {
		t.Fatal("bucket not covered")
	}
	r2 := credentials.Config{Provider: "r2", Bucket: "b", R2AccountID: "acct", R2CredentialRef: "setup-1"}
	r2b := r2
	r2b.R2CredentialRef = "setup-2"
	if DestinationID(r2) != DestinationID(r2b) {
		t.Fatal("a credential reference changed the destination")
	}
	r2b.R2AccountID = "other"
	if DestinationID(r2) == DestinationID(r2b) {
		t.Fatal("endpoint not covered")
	}
}

// New projects are added included, activated at the import, in the plan's
// spelling; apps without hooks are added to ImportedHarnesses.
func TestApplyToConfigAndClock(t *testing.T) {
	admitted := fixedNow.UTC()
	cfg := config.Config{Harnesses: []string{"claude"}, Archive: archive.Config{Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID("/work/in"), Root: "/work/in", Included: true, ActivatedAt: admitted.Add(-time.Hour)},
	}}}
	p := Plan{Home: "/Users/p", Harnesses: cfg.Harnesses, Candidates: []Candidate{
		{Harness: "claude", ProjectRoot: "/work/in", ProjectIncluded: true},
		{Harness: "codex", ProjectRoot: "/work/new"},
		{Harness: "claude", ProjectRoot: "/work/new"},
		{Harness: "cursor", ProjectRoot: "/work/skipped", Skip: SkipTemporaryDirectory},
	}}
	if err := CheckClock(cfg, p, admitted); err != nil {
		t.Fatal(err)
	}
	if err := CheckClock(cfg, p, admitted.Add(-2*time.Hour)); err == nil || !strings.Contains(err.Error(), "check the clock") {
		t.Fatalf("clock before activation: %v", err)
	}
	cfg.DestinationSince = admitted.Add(time.Minute)
	if err := CheckClock(cfg, p, admitted); err == nil {
		t.Fatal("clock before the destination accepted")
	}
	cfg.DestinationSince = time.Time{}

	projects, apps := ApplyToConfig(&cfg, p, admitted)
	if len(projects) != 1 || projects[0] != archive.ProjectID("/work/new") || strings.Join(apps, ",") != "codex" {
		t.Fatalf("added %v, %v", projects, apps)
	}
	added := cfg.Archive.Projects[1]
	if added.Root != "/work/new" || !added.Included || !added.ActivatedAt.Equal(admitted) {
		t.Fatalf("%+v", added)
	}
	if again, apps := ApplyToConfig(&cfg, p, admitted); len(again) != 0 || len(apps) != 0 || len(cfg.Archive.Projects) != 2 {
		t.Fatalf("applied twice: %v %v %+v", again, apps, cfg.Archive.Projects)
	}
}

// Registration skips, and counts, what changed since the plan: a transcript
// that is gone, a session registered meanwhile, and a project no longer
// admitted. Pausing stops it.
func TestRegistrationSkipsChanges(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
		{ProjectID: "x", Root: "/work/excluded", Included: false},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := collector.NewLocalStore(home)
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
	ok, gone, taken, excluded := candidate("ok", project), candidate("gone", project), candidate("taken", project), candidate("excluded", "/work/excluded")
	if err := os.Remove(gone.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterNewSession("taken", func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "taken", ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "claude"}, SessionStartedAt: admitted}
	}); err != nil {
		t.Fatal(err)
	}
	r := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted}
	result, err := r.Run([]Candidate{ok, gone, taken, excluded})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 || result.Gone != 1 || result.AlreadyArchived != 1 || result.NotAdmitted != 1 {
		t.Fatalf("%+v", result)
	}
	reg, _, _ := store.LoadRegistration(result.Sessions[0])
	if reg.Origin != archive.SessionOriginImport || reg.ImportBatch != "2026-09-23-1" || !reg.AdmittedAt.Equal(admitted) || reg.NativeSessionID != "ok" {
		t.Fatalf("%+v", reg)
	}

	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run([]Candidate{candidate("later", project)}); err != ErrPaused {
		t.Fatalf("paused: %v", err)
	}
}
