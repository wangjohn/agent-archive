package backfill

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A chat found only in Cursor's database registers with its source (the
// chat, by ID) and no transcript path; one Cursor deleted since the plan is
// counted gone and not registered.

func TestRegistrationOfCursorDatabaseChats(t *testing.T) {
	home, project, userHome := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	admitted := fixedNow.UTC()
	bucket := credentials.Config{Provider: "s3", Bucket: "a"}
	cfg := config.Config{Storage: bucket, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	db := CursorStateDatabase(userHome)
	writeCursorDB(t, db, true, chatRows("here", nil, "hi"))
	candidate := func(id string) Candidate {
		return Candidate{Harness: "cursor", NativeSessionID: id, SourceKind: archive.SourceKindCursorSQLite, SourceKey: id, ProjectRoot: project,
			StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceCursorComposer}
	}
	// Cursor's database is never read while hooks.lock is held: a hook waits
	// at most a second for it, and a read can wait on Cursor's own lock.
	reads := 0
	sources := observedSources{before: func() {
		reads++
		release, err := local.NamedLock(home, "hooks.lock")
		if err != nil {
			t.Errorf("source read while hooks.lock is held (%v)", err)
		} else {
			release()
		}
	}}
	r := Registration{Sources: sources, Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted, DestinationID: config.DestinationID(bucket), CursorDatabase: db}
	result, err := r.Run([]Candidate{candidate("here"), candidate("deleted")})
	if err != nil || len(result.Sessions) != 1 || result.Gone != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	reg, _, _ := store.LoadRegistration(result.Sessions[0])
	if reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != "here" || reg.TranscriptPath != "" ||
		reg.DestinationID != config.DestinationID(bucket) || reg.Origin != archive.SessionOriginImport || reg.StartedAtSource != archive.StartedAtSourceCursorComposer {
		t.Fatalf("registration %+v", reg)
	}
	if reads == 0 {
		t.Fatal("Cursor's database was never checked")
	}
	if _, pending, err := store.LoadPending(reg.ArchiveSessionID); err != nil || pending {
		t.Fatalf("pending publication %v %v", pending, err)
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("requests %+v %v", requests, err)
	}
}

type observedSources struct{ before func() }

func (s observedSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	return observedProvider{p, s.before}, f, ok
}

type observedProvider struct {
	agentapi.SourceProvider
	before func()
}

func (p observedProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return observedPass{pass, p.before}, nil
}

type observedPass struct {
	agentapi.SourcePass
	before func()
}

func (p observedPass) Signature(ctx context.Context, r agentapi.SourceRef) (agentapi.SourceObservation, error) {
	p.before()
	return p.SourcePass.Signature(ctx, r)
}
