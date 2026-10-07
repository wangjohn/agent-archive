package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
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
	if err != nil || !found || journal.Phase != "cleaned" {
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
	result := collect(t, local, remote, now.Add(time.Minute))
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	restored := fetchMetadata(t, remote, reg.ArchiveSessionID)
	if restored.SourceBundle != previous.SourceBundle {
		t.Fatal("restoration changed frozen source")
	}
	loaded, err := reader.LoadSource(t.Context(), remote, restored, reader.Limits{})
	if err != nil || !strings.Contains(stringProviderJSON(t, loaded), "visible") {
		t.Fatal("restored content unavailable", err)
	}
	journal, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || journal.Phase != "restored" {
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

func TestProviderRestorationMissingOrChangedIntentRetainsPending(t *testing.T) {
	for _, kind := range []string{"missing", "changed"} {
		t.Run(kind, func(t *testing.T) {
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
			if kind == "missing" {
				if err := os.Remove(filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID+".json")); err != nil {
					t.Fatal(err)
				}
			} else {
				published, err := local.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := local.PrepareSessionDeletion(reg, state.RemovalReasonUndo, published.Metadata(), now.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if result := collect(t, local, remote, now.Add(3*time.Minute)); len(result.Errors) == 0 {
				t.Fatal("missing/changed intent silently replayed")
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
