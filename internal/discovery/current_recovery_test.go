package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This test combines two independently supported recovery transitions: a
// registered continuation outside the fresh-start window, and loss of both
// derived indexes while the authoritative registration and census survive.
func TestMovedDiscoveryContinuationRecoversOwnerOutsideNewStartWindow(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	native := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	if h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport); err != nil || h.Registered != 1 {
		t.Fatalf("initial admission: %#v %v", h, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("initial owner: %#v %v", regs, err)
	}
	before := regs[0]
	prior := cfg
	d := *cfg.Discovery
	d.CodexHomes = append([]string{}, cfg.Discovery.CodexHomes...)
	d.CodexHomes = append(d.CodexHomes, t.TempDir())
	cfg.Discovery = &d
	if err := config.ReconcileDiscovery(&cfg, prior, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, fresh := cfg.DiscoveryGeneration("codex", project, before.SessionStartedAt, at.Add(4*time.Minute)); fresh {
		t.Fatal("fixture failed to close original fresh-start window")
	}
	archived := filepath.Join(root, "archived_sessions", filepath.Base(before.TranscriptPath))
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(before.TranscriptPath, archived); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"sessions", "sessions-v1"} {
		if err := os.RemoveAll(filepath.Join(store.Home(), directory)); err != nil {
			t.Fatal(err)
		}
	}
	// The production CLI reopens its Store before every scheduled pass,
	// recreating lost derived directories before recovery can inspect them.
	store, err = state.Open(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated pre-consent original must remain unregistered even when
	// census repair restores the existing admitted owner's derived indexes.
	unknown := writeRollout(t, root, project, at.Add(-time.Hour), 2, "sessions")
	options := Options{Now: func() time.Time { return at.Add(4 * time.Minute) }}
	for range 4 {
		if h, err := runScheduledSynthetic(context.Background(), store, cfg, options); err != nil || h.Registered != 0 {
			t.Fatalf("recovery allocated new identity: %#v %v", h, err)
		}
	}
	owner, found, err := store.ArchiveSessionID(sessionKey("codex", native))
	if err != nil || !found || owner != before.ArchiveSessionID {
		t.Fatalf("existing admitted owner not recovered: owner=%q found=%v err=%v", owner, found, err)
	}
	after, found, err := store.LoadRegistration(owner)
	if err != nil || !found || after.TranscriptPath != archived || after.ArchiveSessionID != before.ArchiveSessionID || after.Origin != before.Origin || !after.SessionStartedAt.Equal(before.SessionStartedAt) || !after.AdmittedAt.Equal(before.AdmittedAt) || after.DiscoveryGeneration != before.DiscoveryGeneration || after.DestinationID != before.DestinationID || after.DiscoveryCwd != before.DiscoveryCwd {
		t.Fatalf("relocation changed immutable attribution or retained missing source: %#v %v", after, err)
	}
	if _, found, err := store.ArchiveSessionID(sessionKey("codex", unknown)); found || err != nil && !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("old unknown history allocated: found=%v err=%v", found, err)
	}
	regs, err = store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("recovery lost or duplicated registration: %#v %v", regs, err)
	}
	result, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: options.Now})
	if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("restored continuation did not publish: %#v %v", result, err)
	}
}

// A legitimate completed empty census must not be treated as damaged solely
// because both derived directories remain empty. Ordinary historical unknown
// sources must converge without repeatedly starting complete census work.
func TestCompletedEmptyCensusDoesNotRepeatForHistoricalUnknownSources(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	if err := store.RecoverSessionIndexIfNeeded(context.Background()); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 8; n++ {
		writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), n, "sessions")
	}
	var priorGeneration string
	options := Options{Now: func() time.Time { return at.Add(4 * time.Minute) }}
	for pass := range 5 {
		if h, err := runScheduledSynthetic(context.Background(), store, cfg, options); err != nil || h.Registered != 0 {
			t.Fatalf("historical unknown admitted: %#v %v", h, err)
		}
		var marker struct {
			Generation string `json:"generation"`
			Complete   bool   `json:"complete"`
		}
		if err := local.Read(filepath.Join(store.Home(), "session-index.json"), &marker); err != nil {
			t.Fatal(err)
		}
		// One initial request/census pair is permitted; after its completed
		// certificate stable history must not change recovery generation.
		if pass >= 2 && (!marker.Complete || marker.Generation != priorGeneration) {
			t.Fatalf("stable historical unknown repeatedly invalidated census: pass=%d marker=%#v", pass, marker)
		}
		priorGeneration = marker.Generation
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("historical unknown produced registrations: %#v %v", regs, err)
	}
}
