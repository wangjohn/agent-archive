package backfill

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// A chat found only in Cursor's database registers with its source (the
// chat, by ID) and no transcript path; one Cursor deleted since the plan is
// counted gone and not registered.
func TestRegistrationOfCursorDatabaseChats(t *testing.T) {
	home, project, userHome := t.TempDir(), t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	bucket := credentials.Config{Provider: "s3", Bucket: "a"}
	cfg := config.Config{Storage: bucket, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	db := CursorStateDatabase(userHome)
	writeCursorDB(t, db, true, chatRows("here", nil, "hi"))
	candidate := func(id string) Candidate {
		return Candidate{Harness: "cursor", NativeSessionID: id, SourceKind: archive.SourceKindCursorSQLite, SourceKey: id, ProjectRoot: project,
			StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceCursorComposer}
	}
	r := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted, DestinationID: config.DestinationID(bucket), CursorDatabase: db}
	result, err := r.Run([]Candidate{candidate("here"), candidate("deleted")})
	if err != nil || len(result.Sessions) != 1 || result.Gone != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	reg, _, _ := store.LoadRegistration(result.Sessions[0])
	if reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != "here" || reg.TranscriptPath != "" ||
		reg.DestinationID != config.DestinationID(bucket) || reg.Origin != archive.SessionOriginImport || reg.StartedAtSource != archive.StartedAtSourceCursorComposer {
		t.Fatalf("registration %+v", reg)
	}
	if _, pending, err := store.LoadPending(reg.ArchiveSessionID); err != nil || pending {
		t.Fatalf("pending publication %v %v", pending, err)
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("requests %+v %v", requests, err)
	}
}
