package cli

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// Regression: 2026-09 review M-20. Every command that counts pending
// sessions (status's pending count and its imports line, setup's
// destination guard, uninstall's warning, backfill's upload progress) asks
// state.Outstanding, so for any one session's state they agree. Before,
// each had its own definition: a published import with an update waiting
// for the upload interval was pending to setup but not to status's imports
// line.
func TestEveryPendingCountAgrees(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	bundle := func(reg archive.SessionRegistration) archive.SourceBundle {
		return archive.SourceBundle{ArchiveSessionID: reg.ArchiveSessionID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: now.Add(-time.Hour)}}
	}
	pendingUpload := func(t *testing.T, store *state.Store, reg archive.SessionRegistration) {
		t.Helper()
		if err := store.SavePending(reg.ArchiveSessionID, state.PendingPublication{
			Bundle: bundle(reg), SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1}, ReadyAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cache := func(t *testing.T, store *state.Store, reg archive.SessionRegistration, status state.CacheStatus) {
		t.Helper()
		if err := statetest.SavePublished(store, reg.ArchiveSessionID, bundle(reg), now.Add(-time.Hour), status); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, store *state.Store, reg archive.SessionRegistration)
		pending bool
	}{
		{"never captured", func(*testing.T, *state.Store, archive.SessionRegistration) {}, true},
		{"published", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
		}, false},
		{"published with a rate-limited update", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			cache(t, s, reg, state.CacheStatusRateLimited)
			pendingUpload(t, s, reg)
		}, true},
		{"published with a rate-limited cache but no upload file", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			cache(t, s, reg, state.CacheStatusRateLimited)
		}, true},
		{"published with a request queued", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			if err := s.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"published with an interrupted scan", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"published with an upload in flight", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			pendingUpload(t, s, reg)
		}, true},
		{"declined", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusDeclined)
		}, false},
		{"request queued in another destination", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			reg.DestinationID = "another-destination"
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"blocked", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			if err := statetest.SaveBlocked(s, reg.ArchiveSessionID, bundle(reg), now.Add(-time.Hour), state.BlockedReasonTranscriptMissing); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			cfg := pairTestConfig(now, []string{"codex"}, project)
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			reg := saveImportedSession(t, store, now, "imported", project)
			tc.setup(t, store, reg)
			if reg, _, err = store.LoadRegistration(reg.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.pending {
				want = 1
			}

			counts := map[string]int{}
			if n, err := pendingSessions(home, cfg); err != nil {
				t.Fatal(err)
			} else {
				counts["setup"] = n
			}
			n, unreadable := unpublishedSessions(home, cfg, true)
			if len(unreadable) > 0 {
				t.Fatal(unreadable)
			}
			counts["uninstall"] = n
			view, err := readStatus(pairStatusEnv(t, home, userHome, now, "codex"))
			if err != nil {
				t.Fatal(err)
			}
			counts["status pending"] = view.Collector.PendingCount
			counts["status imports"] = view.ImportedPending
			if p, err := importPending(store, cfg, reg); err != nil {
				t.Fatal(err)
			} else if p {
				counts["backfill upload"] = 1
			} else {
				counts["backfill upload"] = 0
			}
			for who, got := range counts {
				if got != want {
					t.Errorf("%s counts %d pending, want %d (all: %v)", who, got, want, counts)
				}
			}
		})
	}
}
