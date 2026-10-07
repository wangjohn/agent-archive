package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// uncertainProviderCommit reports a lost acknowledgement only AFTER a real
// provider successfully commits metadata. This is fault injection, not a fake S3.
type uncertainProviderCommit struct {
	storage.ObjectStore
	key  string
	lost bool
}

func (s *uncertainProviderCommit) Put(ctx context.Context, key string, body []byte) error {
	if err := s.ObjectStore.Put(ctx, key, body); err != nil {
		return err
	}
	if key == s.key && !s.lost {
		s.lost = true
		return errors.New("injected lost acknowledgement after actual provider commit")
	}
	return nil
}

func TestProviderAdmissionSurvivesNativeLossAndUncertainCommit(t *testing.T) {
	remote := storagetest.NewDisposableS3(t)
	local, reg := stagedFixture(t)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	uncertain := &uncertainProviderCommit{ObjectStore: remote, key: key}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-provider", RepoKey: func(string) string { t.Fatal("staged publication reopened Git"); return "" }}
	result, err := Run(t.Context(), local, uncertain, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || !uncertain.lost {
		t.Fatal("uncertain actual metadata commit was not observed", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal("uncertain commit lost replay", found, err)
	}
	body, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(body, pending.MetadataBytes) {
		t.Fatal("actual provider did not commit before injected error", err)
	}
	local, err = state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	bundle, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{})
	if err != nil || len(bundle.NativeRecords) == 0 {
		t.Fatal("retained source unreadable after native deletion/restart", err)
	}
	if released, err := local.AdmissionStageReleased(reg); err != nil || !released {
		t.Fatal("verified publication failed to release stage", released, err)
	}
	before := remote.Requests()
	if err := storage.VerifySourceSet(t.Context(), remote, pendingProviderSources(pending), storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
	if remote.Requests()-before > 4 {
		t.Fatal("single selected source verification exceeded actual provider budget")
	}
}

func pendingProviderSources(p state.PendingPublication) []storage.SourcePublication {
	sources := make([]storage.SourcePublication, len(p.Sources))
	for i, source := range p.Sources {
		payload := source.Bytes
		if i == 0 {
			payload = p.SourceBytes
		}
		sources[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes, Bytes: payload}
	}
	return sources
}

func seedProviderFixture(t *testing.T, target, source storage.ObjectStore) {
	t.Helper()
	objects, err := source.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		body, err := source.Get(t.Context(), object.Key)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Put(t.Context(), object.Key, body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderFullSetPrivacyReadbackAndIndependentWinner(t *testing.T) {
	remote := storagetest.NewDisposableS3(t)
	scan, previous, raw := retainedHistoryFixture(t)
	seedProviderFixture(t, remote, scan.remote)
	scan.remote = remote
	pending, err := scan.prepareRetainedPrivacy(raw, remoteRetainedLoader(remote, previous), scan.published.PublicationPredecessor(), state.PrivacyCommitted, "", "", "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SavePending(scan.id(), pending); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), remote, pendingProviderSources(pending), pending.MetadataKey, pending.MetadataBytes, storage.MetadataPredecessor{Known: true, Exists: true, SHA256: pending.Commit.PredecessorSHA256}, storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SaveCommittedPublication(pending, scan.now); err != nil {
		t.Fatal(err)
	}
	var next archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &next); err != nil {
		t.Fatal(err)
	}
	selections, err := state.RevisionSelections(next)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range selections {
		bundle, err := reader.LoadRevisionSource(t.Context(), remote, next, selection, reader.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(bundle)
		if err != nil || strings.Contains(string(body), "obsolete-private-marker") || !strings.Contains(string(body), "synthetic retained prompt") {
			t.Fatal("actual provider retained private bytes or lost content", err)
		}
	}
	if !next.CapturedAt.Equal(previous.CapturedAt) || !next.History.Preserved[0].CapturedAt.Equal(previous.History.Preserved[0].CapturedAt) {
		t.Fatal("privacy rewrite changed retained ages")
	}
	if err := scan.recordPublicationRetirement(pending); err != nil {
		t.Fatal(err)
	}
	assertProviderIndependentWinner(t, remote, pending, next)
}

func assertProviderIndependentWinner(t *testing.T, remote *storagetest.DisposableS3, p state.PendingPublication, winner archive.Metadata) {
	t.Helper()
	// A second local owner observes the same actual provider and selects a
	// different valid sidecar. The stale original owner's exact pending refuses.
	winner.MetadataDerivedAt = winner.MetadataDerivedAt.Add(time.Minute)
	body, err := json.Marshal(winner)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), p.MetadataKey, body); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), remote, pendingProviderSources(p), p.MetadataKey, p.MetadataBytes, storage.MetadataPredecessor{Known: true, Exists: true, SHA256: p.Commit.PredecessorSHA256}, storage.RetryPolicy{}); !errors.Is(err, storage.ErrPublicationConflict) {
		t.Fatal("independent winner overwritten", err)
	}
	if err := storage.VerifySourceSet(t.Context(), remote, pendingProviderSources(p), storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(t.Context(), remote, p.MetadataKey, body); err != nil {
		t.Fatal(err)
	}
	got, err := remote.Get(t.Context(), p.MetadataKey)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("listing changed authoritative winner", err)
	}
	if _, err := reader.LoadSource(t.Context(), remote, winner, reader.Limits{}); err != nil {
		t.Fatal("winner unreadable without local state", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := remote.Requests()
	if err := storage.VerifySourceSet(ctx, remote, pendingProviderSources(p), storage.RetryPolicy{}); !errors.Is(err, context.Canceled) || remote.Requests() != before {
		t.Fatal("cancelled verification performed provider work", err)
	}
}

func TestProviderImmutableMismatchNeverOverwritesSource(t *testing.T) {
	remote := storagetest.NewDisposableS3(t)
	body := []byte("synthetic expected immutable source")
	key := "sources/" + storage.SHA256Hex(body) + ".gz"
	corrupt := []byte("synthetic different bytes")
	if err := remote.Put(t.Context(), key, corrupt); err != nil {
		t.Fatal(err)
	}
	err := storage.PutSourceSetThenMetadata(t.Context(), remote, []storage.SourcePublication{{Key: key, SHA256: storage.SHA256Hex(body), Size: len(body), Bytes: body}}, "metadata.json", []byte("synthetic next metadata"), storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{})
	if !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal("immutable mismatch not refused", err)
	}
	got, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(got, corrupt) {
		t.Fatal("mismatching existing object was overwritten", err)
	}
	if _, err := remote.Get(t.Context(), "metadata.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("corrupt source selected", err)
	}
}
