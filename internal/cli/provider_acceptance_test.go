package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

func TestProviderPrivacyRetirementRequiresVerifiedFullSelection(t *testing.T) {
	remote := providertest.NewDisposableS3(t)
	now := time.Now().UTC()
	// Ordinary CLI cleanup exercises the actual verifier and public Sweep.
	// Full historical retirement is a separate package-local inner-port proof.
	home, project := t.TempDir(), t.TempDir()
	cfg, store := publishSyntheticSessions(t, home, project, remote, now.Add(-48*time.Hour), 1)
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatal("ordinary fixture registration missing", err)
	}
	reg := regs[0]
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	active, _, found := published.LastPublished()
	if !found || active.History != nil {
		t.Fatal("fixture is not an ordinary publication")
	}
	obsolete := active
	obsolete.NativeRecords = []map[string]any{{"type": "turn_context", "model": "obsolete synthetic provider content"}}
	encoded, err := archive.BuildCompressedSource(obsolete)
	if err != nil {
		t.Fatal(err)
	}
	obsoleteKey, err := archive.SourceObjectKey(obsolete, encoded.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Put(t.Context(), obsoleteKey, encoded.Bytes); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordSupersededWithPrivacy(reg.ArchiveSessionID, obsoleteKey, now.Add(-48*time.Hour), true); err != nil {
		t.Fatal(err)
	}
	opts := retention.Options{Now: func() time.Time { return now }, GracePeriod: time.Hour, CurrentDestination: cfg.InCurrentDestination, PrivacyVerified: func(reg archive.SessionRegistration, m archive.Metadata) bool {
		return privacyPublicationVerified(home, cfg, store, reg, m)
	}}
	result, err := retention.Sweep(t.Context(), store, remote, opts)
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatal("unverified cleanup", result, err)
	}
	if _, err = remote.Get(t.Context(), obsoleteKey); err != nil {
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
	if _, err = remote.Get(t.Context(), obsoleteKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("privacy source remains", err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := reader.ReadMetadata(t.Context(), remote, key)
	if err != nil || metadata.History != nil {
		t.Fatal("ordinary selecting publication changed", err)
	}
	bundle, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(bundle.NativeRecords)
	if err != nil || !bundle.Capture.CapturedAt.Equal(active.Capture.CapturedAt) || !strings.Contains(string(body), "synthetic-0") {
		t.Fatal("retirement lost ordinary selected content or age", err)
	}
}
