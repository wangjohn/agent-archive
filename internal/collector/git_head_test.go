package collector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
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

// stopAtNewCommit records laterCommit as the session's last HEAD and a stop
// request with no new evidence, as a stop with no new transcript bytes leaves.
func stopAtNewCommit(t *testing.T, local *state.Store, reg archive.SessionRegistration, at time.Time) *archive.GitHead {
	t.Helper()
	last := &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: at}
	if _, err := local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.LastHead = last; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	return last
}

// A publication made before metadata was cached locally still gets the
// commit a later stop recorded, at the same parser version.
func TestStopCommitPublishesForLegacyPublication(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), ObservedAt: reg.RegisteredAt}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	publishOnce(t, local, remote, reg, &opts)
	if err := statetest.CacheMetadata(local, reg.ArchiveSessionID, nil); err != nil {
		t.Fatal(err)
	}
	last := stopAtNewCommit(t, local, reg, now.Add(time.Minute))
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("legacy publication lost the stop commit: %+v", after.GitHead)
	}
}

// A HEAD-only publication whose source has gone from storage re-uploads the
// retained source instead of failing on every pass.
func TestStopCommitRepairsAMissingSource(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), ObservedAt: reg.RegisteredAt}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)
	if err := remote.Delete(context.Background(), before.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	last := stopAtNewCommit(t, local, reg, now.Add(time.Minute))
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("stop commit not published: %+v", after.GitHead)
	}
	if _, err := remote.Get(context.Background(), after.SourceBundle.Key); err != nil {
		t.Fatalf("source %s not restored: %v", after.SourceBundle.Key, err)
	}
}

// A stop that brings a new commit and new hook evidence after the transcript
// was deleted ends in a gap that completes its request; the commit is still
// published on a later pass, from the retained source.
func TestStopCommitPublishesAfterTheTranscriptGoes(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.StartHead = &archive.GitHead{SHA: strings.Repeat("3f", 20), ObservedAt: reg.RegisteredAt}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: (&countingLookup{}).lookup, Now: func() time.Time { return now }}
	publishOnce(t, local, remote, reg, &opts)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	last := &archive.GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: now.Add(time.Minute)}
	if _, err := local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.LastHead = last; return nil }); err != nil {
		t.Fatal(err)
	}
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: last.ObservedAt, Provenance: "hook", Payload: map[string]any{"turn_id": "t1"}}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", last.ObservedAt, evidence); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		now = now.Add(time.Hour)
		if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
			t.Fatalf("run: %+v %v", result, err)
		}
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("stop commit stranded behind the gap: %+v", after.GitHead)
	}
}

// A commit recorded on the registration after a pass had already completed
// the stop's request (the hook's two writes straddling the pass's listing)
// is published by a later pass with no request at all, and once published
// the session is skipped again.
func TestStopCommitPublishesWithoutARequest(t *testing.T) {
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
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("run: %+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.GitHead == nil || after.GitHead.Last == nil || after.GitHead.Last.SHA != last.SHA {
		t.Fatalf("stop commit not published: %+v", after.GitHead)
	}
	remote.keys = nil
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("second run: %+v %v", result, err)
	}
	if len(remote.keys) != 0 {
		t.Errorf("a published commit was published again: %v", remote.keys)
	}
}
