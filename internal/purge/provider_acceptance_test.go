package purge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

func TestProviderPurgePreservesMaximumRetainedSetAndContent(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	now := time.Now().UTC()
	f := providertest.PutRetainedFixture(t, remote, archive.MaxPreservedRevisions, now)
	plan, err := Inventory(t.Context(), remote, "synthetic-destination", "aa-disposable-acceptance", "", ModeUnreferenced, "", now)
	if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].Key != f.Unreferenced.Key {
		t.Fatal("purge did not isolate the unselected source", plan.Candidates, err)
	}
	report := Report{PlanDigest: plan.Digest, Remaining: []string{f.Unreferenced.Key}}
	reportPath := filepath.Join(t.TempDir(), "purge-report.json")
	if err := Apply(t.Context(), remote, plan, &report, func(r Report) error { return local.Write(reportPath, r) }); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(t.Context(), f.Unreferenced.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("unselected source remains", err)
	}
	assertProviderRetainedContent(t, remote, f.Metadata)
}

func assertProviderRetainedContent(t *testing.T, remote storage.ObjectStore, metadata archive.Metadata) {
	t.Helper()
	selected := []archive.RevisionReference{{RevisionID: metadata.History.CurrentRevision, CapturedAt: metadata.CapturedAt, Source: metadata.SourceBundle}}
	selected = append(selected, metadata.History.Preserved...)
	for _, ref := range selected {
		bundle, err := reader.LoadRevisionSource(t.Context(), remote, metadata, ref, reader.Limits{})
		if err != nil || !bundle.Capture.CapturedAt.Equal(ref.CapturedAt) {
			t.Fatal("selected retained source lost identity or capture age", err)
		}
		body, err := json.Marshal(bundle)
		if err != nil || !strings.Contains(string(body), "synthetic retained provider content") {
			t.Fatal("selected retained content was lost", err)
		}
	}
}

type unreadableProviderMetadata struct {
	storage.ObjectStore
	key string
}

func (s unreadableProviderMetadata) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := s.ObjectStore.Get(ctx, key)
	if err == nil && key == s.key {
		return nil, errors.New("injected unreadable acknowledgement after actual metadata GET")
	}
	return data, err
}

func TestProviderPurgeAmbiguousSelectingMetadataFailsClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "incomplete", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			remote := providertest.NewDisposableS3(t)
			f := providertest.PutRetainedFixture(t, remote, 2, time.Now().UTC())
			key, err := archive.MetadataObjectKey(f.Registration.Harness.Name, f.Registration.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			var observed storage.ObjectStore = remote
			switch kind {
			case "corrupt":
				if err := remote.Put(t.Context(), key, []byte("{")); err != nil {
					t.Fatal(err)
				}
			case "incomplete":
				if err := remote.Delete(t.Context(), f.Metadata.History.Preserved[0].Source.Key); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				observed = unreadableProviderMetadata{ObjectStore: remote, key: key}
			}
			if _, err := Inventory(t.Context(), observed, "synthetic-destination", "aa-disposable-acceptance", "", ModeUnreferenced, "", time.Now().UTC()); err == nil {
				t.Fatal("ambiguous selecting evidence produced a purge plan")
			}
			if _, err := remote.Get(t.Context(), f.Unreferenced.Key); err != nil {
				t.Fatal("failed inventory removed recoverable unselected evidence", err)
			}
			if _, err := remote.Get(t.Context(), f.Metadata.SourceBundle.Key); err != nil {
				t.Fatal("failed inventory removed current evidence", err)
			}
		})
	}
}
