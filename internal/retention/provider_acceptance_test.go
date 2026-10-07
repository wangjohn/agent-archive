package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

func TestProviderOwnedDeletionResumesUncertainAcknowledgement(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	at := time.Now().UTC().Add(-48 * time.Hour)
	reg := registration("provider-owned-delete", writeTranscript(t, t.TempDir(), "source.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result := collect(t, local, remote, at)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	meta := fetchMetadata(t, remote, reg.ArchiveSessionID)
	if err := DeleteOwnedSession(t.Context(), local, &uncertainMetadataDelete{ObjectStore: remote}, reg, state.RemovalReasonUndo, time.Now().UTC()); err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	if _, err := remote.Get(t.Context(), meta.SourceBundle.Key); err != nil {
		t.Fatal("source lost before durable absence", err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	if err = DeleteOwnedSession(t.Context(), restarted, remote, reg, state.RemovalReasonUndo, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assertProviderNamespaceAbsent(t, remote, reg)
	journal, found, err := restarted.LoadSessionDeletion(reg)
	if err != nil || !found || journal.Phase != state.DeletionCleaned {
		t.Fatal(journal, err)
	}
}

func TestProviderRetentionExpiryUsesServiceClock(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	now := time.Now().UTC()
	reg := registration("provider-expiry", writeTranscript(t, t.TempDir(), "source.jsonl", codexTranscript))
	reg.SessionStartedAt = now.Add(-48 * time.Hour)
	reg.RegisteredAt = reg.SessionStartedAt
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, reg.RegisteredAt); len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	result, err := Sweep(t.Context(), local, remote, Options{Now: func() time.Time { return now }, SessionMaxAge: 24 * time.Hour})
	if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
		t.Fatal(result, err)
	}
	assertProviderNamespaceAbsent(t, remote, reg)
	journal, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || !journal.LocalRemoved {
		t.Fatal("local cleanup not terminal", journal, err)
	}
}

func TestProviderRetentionRestoresAfterNewHook(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	at := time.Now().UTC().Add(-48 * time.Hour)
	reg := registration("provider-restore", writeTranscript(t, t.TempDir(), "source.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, at); len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	previous := fetchMetadata(t, remote, reg.ArchiveSessionID)
	now := time.Now().UTC()
	if err := DeleteOwnedSession(t.Context(), local, &uncertainMetadataDelete{ObjectStore: remote}, reg, state.RemovalReasonRetention, now); err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", now, finalResponse(t, now)); err != nil {
		t.Fatal(err)
	}
	if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonRetention, now); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(t.Context(), previous.SourceBundle.Key); err != nil {
		t.Fatal("new hook lost restoration source", err)
	}
	if result := collect(t, local, &uncertainRestorationPut{ObjectStore: remote}, now.Add(time.Minute)); len(result.Errors) == 0 {
		t.Fatal("lost restoration acknowledgement accepted")
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || pending.ValidatePublication() != nil {
		t.Fatal("sealed restoration successor missing", err)
	}
	result := collect(t, local, remote, now.Add(2*time.Minute))
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	selected, err := remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || !bytes.Equal(selected, pending.MetadataBytes) {
		t.Fatal("restoration changed sealed selecting successor", err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || !bytes.Equal(published.Metadata(), pending.MetadataBytes) {
		t.Fatal("local successor differs from verified remote selection", err)
	}
	refs, err := published.CommittedSources()
	var restored archive.Metadata
	if err != nil || json.Unmarshal(selected, &restored) != nil {
		t.Fatal("invalid restored selection", err)
	}
	selectedRefs, err := restored.SourceReferences()
	if err != nil || !slices.Equal(refs, selectedRefs) {
		t.Fatal("restoration lost exact sealed full set", err)
	}
	if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("verified successor did not settle pending restoration", err)
	}
	loaded, err := reader.LoadSource(t.Context(), remote, restored, reader.Limits{})
	if err != nil || !strings.Contains(stringProviderJSON(t, loaded), "visible") {
		t.Fatal("restored content unavailable", err)
	}
	journal, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || journal.Phase != state.DeletionRestored {
		t.Fatal(journal, err)
	}
}

// This accepts the existing inner journal/verification/cleanup ports. The
// public historical mutation fence remains an independent enablement gate.
func TestProviderFullSetDeletionInnerPortsPreserveFence(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	fixture := providertest.PutRetainedFixture(t, remote, archive.MaxPreservedRevisions, time.Now().UTC())
	reg := fixture.Registration
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonUndo, time.Now().UTC()); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal("history fence changed", err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SavePublication(fixture.Active, fixture.Metadata.CapturedAt, fixture.Metadata.SourceBundle, fixture.Body); err != nil {
		t.Fatal(err)
	}
	journal, err := local.PrepareSessionDeletion(reg, state.RemovalReasonUndo, fixture.Body, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = deleteOwnedMetadata(t.Context(), local, remote, reg, &journal, key, fixture.Body); err != nil {
		t.Fatal(err)
	}
	refs, err := fixture.Metadata.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if _, err := remote.Get(t.Context(), ref.Key); err != nil {
			t.Fatal("source removed before metadata absence committed", err)
		}
	}
	if err = deleteOwnedNamespace(t.Context(), local, remote, reg, journal, key); err != nil {
		t.Fatal(err)
	}
	assertProviderNamespaceAbsent(t, remote, reg)
}

func assertProviderNamespaceAbsent(t *testing.T, remote storage.ObjectStore, reg archive.SessionRegistration) {
	t.Helper()
	objects, err := remote.List(t.Context(), "sessions/"+reg.Harness.Name+"/"+reg.ArchiveSessionID+"/")
	if err != nil || len(objects) != 0 {
		t.Fatal("owned namespace remains", objects, err)
	}
}

func stringProviderJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

type uncertainRestorationPut struct {
	storage.ObjectStore
	once bool
}

func (s *uncertainRestorationPut) Put(ctx context.Context, key string, body []byte) error {
	err := s.ObjectStore.Put(ctx, key, body)
	if err == nil && strings.HasSuffix(key, "/metadata.json") && !s.once {
		s.once = true
		return errors.New("synthetic lost restoration acknowledgement")
	}
	return err
}

type providerIntentFault string

const (
	providerMissingIntent providerIntentFault = "missing"
	providerChangedIntent providerIntentFault = "changed"
)

func TestProviderRestorationMissingOrChangedIntentRetainsPending(t *testing.T) {
	for _, kind := range []providerIntentFault{providerMissingIntent, providerChangedIntent} {
		t.Run(string(kind), func(t *testing.T) {
			local, remote, reg, now, key, selected := prepareProviderRestoration(t)
			invalidateProviderIntent(t, local, reg, now, kind)
			result := collect(t, local, remote, now.Add(3*time.Minute))
			if len(result.Published) != 0 {
				t.Fatal("missing/changed intent silently replayed", result)
			}
			if kind == providerMissingIntent && len(result.Errors) == 0 {
				t.Fatal("missing intent was not actionable", result)
			}
			if kind == providerChangedIntent {
				work, err := local.Outstanding(reg, true)
				if err != nil || !work.Removal || len(result.Skipped) != 1 {
					t.Fatal("changed removal intent was not durably held", work, result, err)
				}
			}
			if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("invalid intent discarded durable restoration")
			}
			actual, err := remote.Get(t.Context(), key)
			if err != nil || !bytes.Equal(actual, selected) {
				t.Fatal("invalid intent changed remote authority", err)
			}
		})
	}
}

func prepareProviderRestoration(t *testing.T) (*state.Store, *providertest.DisposableS3, archive.SessionRegistration, time.Time, string, []byte) {
	t.Helper()
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	at := time.Now().UTC().Add(-48 * time.Hour)
	reg := registration("provider-intent", writeTranscript(t, t.TempDir(), "source.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, at); len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	now := time.Now().UTC()
	if err := DeleteOwnedSession(t.Context(), local, &uncertainMetadataDelete{ObjectStore: remote}, reg, state.RemovalReasonRetention, now); err == nil {
		t.Fatal("lost delete acknowledgement accepted")
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", now, finalResponse(t, now)); err != nil {
		t.Fatal(err)
	}
	if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonRetention, now); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, &uncertainRestorationPut{ObjectStore: remote}, now.Add(time.Minute)); len(result.Errors) == 0 {
		t.Fatal("lost restore acknowledgement accepted")
	}
	if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("restoration evidence missing")
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return local, remote, reg, now, key, selected
}

func invalidateProviderIntent(t *testing.T, local *state.Store, reg archive.SessionRegistration, now time.Time, kind providerIntentFault) {
	t.Helper()
	switch kind {
	case providerMissingIntent:
		if err := os.Remove(filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID+".json")); err != nil {
			t.Fatal(err)
		}
	case providerChangedIntent:
		published, err := local.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.PrepareSessionDeletion(reg, state.RemovalReasonUndo, published.Metadata(), now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unsupported synthetic intent fault")
	}
}

// This is component acceptance of the shipped inner retirement port. Public
// historical Sweep remains fenced; no enabled outer lifecycle is claimed.
func TestProviderFullSetPrivacyRetirementInnerPortRequiresReadback(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	local := newTestStore(t)
	now := time.Now().UTC()
	f := providertest.PutRetainedFixture(t, remote, archive.MaxPreservedRevisions, now.Add(-48*time.Hour))
	if err := local.SaveRegistration(f.Registration); err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(f.Registration.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SavePublication(f.Active, f.Metadata.CapturedAt, f.Metadata.SourceBundle, f.Body); err != nil {
		t.Fatal(err)
	}
	if err = local.RecordSupersededWithPrivacy(f.Registration.ArchiveSessionID, f.Unreferenced.Key, now.Add(-48*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	ledger, err := local.LoadSuperseded(f.Registration.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: f.Metadata.MachineID}
	verifiedSelection := ""
	opts := Options{GracePeriod: time.Hour, CurrentDestination: cfg.InCurrentDestination, PrivacyVerified: func(reg archive.SessionRegistration, metadata archive.Metadata) bool {
		body, err := json.Marshal(metadata)
		return err == nil && verifiedSelection != "" && storage.SHA256Hex(body) == verifiedSelection && cfg.InCurrentDestination(reg)
	}}
	s := &sweeper{ctx: t.Context(), local: local, store: remote, opts: opts, now: now, metadataSHA: storage.SHA256Hex(f.Body), result: Result{Errors: map[string]error{}}}
	if err = s.deleteSuperseded(f.Registration, ledger, f.Metadata); err != nil || s.result.DeletedSnapshots != 0 {
		t.Fatal("unverified historical privacy cleanup", s.result, err)
	}
	if _, err = remote.Get(t.Context(), f.Unreferenced.Key); err != nil {
		t.Fatal("unverified privacy evidence removed", err)
	}
	verifiedSelection = verifyProviderRetainedSelection(t, local, remote, cfg, f)
	if err = s.deleteSuperseded(f.Registration, ledger, f.Metadata); err != nil || s.result.DeletedSnapshots != 1 {
		t.Fatal("verified full-set retirement failed", s.result, err)
	}
	if _, err = remote.Get(t.Context(), f.Unreferenced.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("obsolete privacy evidence remains", err)
	}
	// Read again after destructive cleanup: every retained body and age survives.
	if after := verifyProviderRetainedSelection(t, local, remote, cfg, f); after != verifiedSelection {
		t.Fatal("retirement changed selecting evidence")
	}
}

func verifyProviderRetainedSelection(t *testing.T, local *state.Store, remote storage.ObjectStore, cfg config.Config, f providertest.RetainedFixture) string {
	t.Helper()
	if !cfg.InCurrentDestination(f.Registration) || cfg.MachineID != f.Metadata.MachineID {
		t.Fatal("selection is outside current owner/destination")
	}
	published, err := local.LoadPublishedState(f.Registration.ArchiveSessionID)
	if err != nil || !bytes.Equal(published.Metadata(), f.Body) {
		t.Fatal("durable selecting metadata differs", err)
	}
	refs, err := published.CommittedSources()
	selectedRefs, selectedErr := f.Metadata.SourceReferences()
	if err != nil || selectedErr != nil || !slices.Equal(refs, selectedRefs) {
		t.Fatal("durable complete selection differs", err, selectedErr)
	}
	key, err := archive.MetadataObjectKey(f.Registration.Harness.Name, f.Registration.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(raw, f.Body) {
		t.Fatal("remote selecting authority differs", err)
	}
	revisions := []archive.RevisionReference{{RevisionID: f.Metadata.History.CurrentRevision, CapturedAt: f.Metadata.CapturedAt, Source: f.Metadata.SourceBundle}}
	revisions = append(revisions, f.Metadata.History.Preserved...)
	for _, revision := range revisions {
		bundle, err := reader.LoadRevisionSource(t.Context(), remote, f.Metadata, revision, reader.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Capture.FilterVersion != archive.FilterVersion || !bundle.Capture.CapturedAt.Equal(revision.CapturedAt) || bundle.History.ActiveRolloutID != revision.RevisionID || !strings.Contains(stringProviderJSON(t, bundle.NativeRecords), "synthetic retained provider content") {
			t.Fatal("retained current-policy identity, content or age differs")
		}
	}
	return storage.SHA256Hex(raw)
}
