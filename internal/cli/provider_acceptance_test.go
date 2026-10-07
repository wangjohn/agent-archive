package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

func TestProviderPrivacyRetirementRequiresVerifiedFullSelection(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	now := time.Now().UTC()
	f := providertest.PutRetainedFixture(t, remote, 2, now.Add(-48*time.Hour))
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveRegistration(f.Registration); err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(f.Registration.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SavePublication(f.Active, f.Metadata.CapturedAt, f.Metadata.SourceBundle, f.Body); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordSupersededWithPrivacy(f.Registration.ArchiveSessionID, f.Unreferenced.Key, now.Add(-48*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: f.Metadata.MachineID, Storage: credentialsTestConfig(), Harnesses: []string{"codex"}}
	opts := retention.Options{Now: func() time.Time { return now }, GracePeriod: time.Hour, CurrentDestination: cfg.InCurrentDestination, PrivacyVerified: func(reg archive.SessionRegistration, m archive.Metadata) bool {
		return privacyPublicationVerified(home, cfg, store, reg, m)
	}}
	result, err := retention.Sweep(t.Context(), store, remote, opts)
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatal("unverified cleanup", result, err)
	}
	if _, err = remote.Get(t.Context(), f.Unreferenced.Key); err != nil {
		t.Fatal("unverified source removed", err)
	}
	summary, err := verifyPublicationsWithin(t.Context(), home, cfg, testEnv(t, home, now), store, remote)
	if err != nil || summary.Verified != 1 {
		t.Fatal("production verification failed", summary, err)
	}
	result, err = retention.Sweep(t.Context(), store, remote, opts)
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatal("verified privacy retirement failed", result, err)
	}
	if _, err = remote.Get(t.Context(), f.Unreferenced.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("privacy source remains", err)
	}
	selected := []archive.RevisionReference{{RevisionID: f.Metadata.History.CurrentRevision, CapturedAt: f.Metadata.CapturedAt, Source: f.Metadata.SourceBundle}}
	selected = append(selected, f.Metadata.History.Preserved...)
	for _, ref := range selected {
		bundle, err := reader.LoadRevisionSource(t.Context(), remote, f.Metadata, ref, reader.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(bundle.NativeRecords)
		if err != nil || !bundle.Capture.CapturedAt.Equal(ref.CapturedAt) || !strings.Contains(string(body), "synthetic retained provider content") {
			t.Fatal("retirement lost selected content or age", err)
		}
	}
}
