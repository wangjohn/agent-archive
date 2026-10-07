package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func unuploadedPrivacyFixture(t *testing.T) (*state.Store, archive.SessionRegistration, *metadataFailStore, Options, state.PendingPublication) {
	t.Helper()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceBody, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }, RepoKey: func(string) string { return "synthetic" }, SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", ObservedAt: at, Payload: map[string]any{"name": "synthetic", "snapshot": "private-original-evidence"}}}, nil
	}}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("expected initial uncertain metadata", result, err)
	}
	old, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || old.Commit == nil {
		t.Fatal(old, found, err)
	}
	if err := remote.Delete(t.Context(), old.SourceKey); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy replay observed native skill inventory")
		return nil, nil
	}
	return local, reg, remote, opts, old
}

func TestPrivacyOriginalJournalSurvivesRestartUntilLocalSuccessor(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("expected successor upload checkpoint", result, err)
	}
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 == "" || next.SourceSHA256 == old.SourceSHA256 {
		t.Fatal(next, found, err)
	}
	raw, err := local.ReadPublicationEvidence(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var journal privacyInputJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal.Original.SourceBytes, old.SourceBytes) || !bytes.Equal(journal.Original.MetadataBytes, old.MetadataBytes) || journal.ReplacementMetadataSHA256 != next.Commit.MetadataSHA256 {
		t.Fatal("original bytes were replaced before local commit")
	}
	if _, err := remote.Get(t.Context(), old.SourceKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("obsolete original reuploaded", err)
	}
	local, err = state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original evidence not retired after exact local commit", err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	source, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("obsolete evidence selected", bundle, err)
	}
}

func TestPrivacyOriginalJournalMissingOrCorruptKeepsObligation(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
			_, _ = Run(t.Context(), local, remote, opts)
			before, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || before.Commit.Privacy.InputJournalSHA256 == "" {
				t.Fatal(before, found, err)
			}
			if kind == "missing" {
				if err := local.RemovePublicationEvidence(reg.ArchiveSessionID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(local.Home(), "publication-evidence", reg.ArchiveSessionID, "journal.json"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			remote.failMetadata = false
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
				t.Fatal("missing original evidence bypassed", result, err)
			}
			after, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || after.Commit.MetadataSHA256 != before.Commit.MetadataSHA256 {
				t.Fatal("pending evidence lost", after, found, err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("uncommitted request completed", found, err)
			}
		})
	}
}

func TestPrivacyOriginalJournalSurvivesFailedLocalCommit(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "published", reg.ArchiveSessionID+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
	if _, err := scan.maintainPendingPrivacy(old); err == nil {
		t.Fatal("synthetic local commit failure was ignored")
	}
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 == "" {
		t.Fatal(next, found, err)
	}
	body, err := remote.Get(t.Context(), next.MetadataKey)
	if err != nil || storage.SHA256Hex(body) != next.Commit.MetadataSHA256 {
		t.Fatal("failed before intended remote commit", err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("remote PUT released original evidence", err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("failed local save acknowledged request", found, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original evidence not cleaned after exact local completion", err)
	}
}

func TestPrivacyEmbeddedRemoteSourceLossKeepsPending(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	// Restore exact immutable original remotely: the replacement may reference it
	// rather than duplicating bytes in a charged journal.
	if err := remote.Put(t.Context(), old.SourceKey, old.SourceBytes); err != nil {
		t.Fatal(err)
	}
	_, _ = Run(t.Context(), local, remote, opts)
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 != "" || next.Commit.Privacy.ReplayInput == nil {
		t.Fatal(next, found, err)
	}
	if err := remote.Delete(t.Context(), old.SourceKey); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal("vanished original backing accepted", result, err)
	}
	after, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || after.Commit.MetadataSHA256 != next.Commit.MetadataSHA256 {
		t.Fatal("source loss discarded pending", after, found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("source loss completed request", found, err)
	}
}

func TestPrivacyOrphanEvidenceKeepsWorkVisibleWithoutNativeFallback(t *testing.T) {
	local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
	_, _ = Run(t.Context(), local, remote, opts)
	if err := local.RemovePending(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	owed, err := local.Outstanding(reg, false)
	if err != nil || !owed.Upload || !owed.Pending() || !owed.DefersExpiry() {
		t.Fatal("original evidence lost work obligation", owed, err)
	}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal("orphan original evidence reopened native", result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("orphan evidence dropped", err)
	}
}

func TestPrivacyPreparedEvidenceQuotaFailureKeepsOriginalPending(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
	prior, kind, err := scan.reconcilePrivacyPending(old)
	if err != nil {
		t.Fatal(err)
	}
	loader, err := scan.pendingRetainedLoader(old)
	if err != nil {
		t.Fatal(err)
	}
	next, err := scan.prepareRetainedPrivacy(old.MetadataBytes, loader, prior, kind, "", "", old.SkillEvidence)
	if err != nil {
		t.Fatal(err)
	}
	next.RequestToken = old.RequestToken
	if err := scan.preservePrivacyInput(old, &next); err != nil {
		t.Fatal(err)
	}
	filler := filepath.Join(local.Home(), "pending", "synthetic-capacity.json")
	f, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(state.AdmissionStageQuota / 2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scan.persistPrivacy(next); !errors.Is(err, state.ErrAdmissionStageCapacity) {
		t.Fatal("expected aggregate quota stop", err)
	}
	current, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || !bytes.Equal(current.SourceBytes, old.SourceBytes) || current.Commit.MetadataSHA256 != old.Commit.MetadataSHA256 {
		t.Fatal("failed replacement discarded original pending", current, found, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("prepared evidence disappeared", err)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("prepared evidence did not reconcile", result, err)
	}
}
