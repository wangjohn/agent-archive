package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A publication carries the commits the session's hooks recorded on its
// registration, and nothing when they recorded none: the collector never
// asks git for a commit itself.
func TestPublicationCarriesTheCommitsTheHooksRecorded(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	dirty := true
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), Dirty: &dirty, ObservedAt: reg.RegisteredAt}
	reg.LastHead = &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: reg.RegisteredAt.Add(time.Minute)}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	got := publishOnce(t, local, remote, reg, &opts).GitHead
	if got == nil || got.Start == nil || got.Start.SHA != reg.StartHead.SHA || got.Start.Dirty == nil || !*got.Start.Dirty ||
		got.Last == nil || got.Last.SHA != reg.LastHead.SHA {
		t.Errorf("git_head = %+v, want the registration's", got)
	}
}

func TestPublicationWithoutRecordedCommitsOmitsGitHead(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	publishOnce(t, local, remote, reg, &opts)
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	raw, err := remote.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "git_head") {
		t.Errorf("a session with no recorded commit carries git_head: %s", raw)
	}
}

func TestStopCommitPublishesWithoutTranscriptGrowth(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), ObservedAt: reg.RegisteredAt}
	remote := &countedPublications{ObjectStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)
	last := &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: now.Add(time.Minute)}
	if _, err := local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.LastHead = last; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", last.ObservedAt); err != nil {
		t.Fatal(err)
	}
	remote.keys = nil
	now = now.Add(time.Hour)
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("stop commit not published: %+v", after.GitHead)
	}
	for _, key := range remote.keys {
		if key == before.SourceBundle.Key {
			t.Errorf("HEAD-only change rewrote source: %s", key)
		}
	}
	if after.SourceBundle != before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) {
		t.Fatalf("HEAD-only change altered source or capture time")
	}
}

// A stop that brings new hook evidence along with a new commit is left to
// normal capture, which folds both into one publication and completes the
// request, instead of a metadata-only update that leaves it outstanding.
func TestStopCommitWithHookEvidencePublishesOnce(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), ObservedAt: reg.RegisteredAt}
	remote := &countedPublications{ObjectStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	publishOnce(t, local, remote, reg, &opts)
	last := &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: now.Add(time.Minute)}
	if _, err := local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.LastHead = last; return nil }); err != nil {
		t.Fatal(err)
	}
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: last.ObservedAt, Provenance: "hook", Payload: map[string]any{"turn_id": "t1"}}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", last.ObservedAt, evidence); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	if _, pending, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || pending {
		t.Fatalf("request still outstanding after one pass (err %v)", err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("stop commit not published: %+v", after.GitHead)
	}
}
