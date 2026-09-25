package retention

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Regression: PR #57 review. Retention's expiry deferral moved onto
// state.Outstanding, and briefly deferred on every kind of owed work,
// including an interrupted scan. A scan that fails on every pass (here, a
// transcript the filter refuses) leaves its journal set for good, so such
// a session was never expired, locally or in the bucket. Only a queued
// request or an upload storage has not accepted defers expiry, as before.
func TestExpiryDefersOnlyForUndeliveredEvidence(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	expiry := t0.Add(retentionWindow + 24*time.Hour)
	for _, tc := range []struct {
		name     string
		after    func(t *testing.T, local *state.Store, path string)
		deferred bool
	}{
		{name: "settled"},
		{
			// The realistic stuck journal: the transcript no longer holds one record the filter recognizes,
			// every pass fails on it, and nothing else is owed.
			name: "scan failing on every pass",
			after: func(t *testing.T, local *state.Store, path string) {
				t.Helper()
				writeTranscript(t, filepath.Dir(path), filepath.Base(path), "not json\n")
				collect(t, local, storage.NewMemoryStore(), t0.Add(time.Hour))
				if o, err := local.Outstanding(registration("s1", path), false); err != nil || !o.Scan {
					t.Fatalf("the failing scan left no journal (%+v, %v); the test no longer covers it", o, err)
				}
			},
		},
		{
			name: "interrupted scan",
			after: func(t *testing.T, local *state.Store, _ string) {
				t.Helper()
				if err := local.SetScanPending("s1", true); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "request queued",
			after: func(t *testing.T, local *state.Store, _ string) {
				t.Helper()
				if err := local.SaveRequest("s1", "stop", expiry); err != nil {
					t.Fatal(err)
				}
			},
			deferred: true,
		},
		{
			name: "upload not accepted",
			after: func(t *testing.T, local *state.Store, _ string) {
				t.Helper()
				published, err := local.LoadPublishedState("s1")
				if err != nil {
					t.Fatal(err)
				}
				bundle, _, _ := published.LastPublished()
				if err := local.SavePending("s1", state.PendingPublication{
					Bundle: bundle, SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1},
				}); err != nil {
					t.Fatal(err)
				}
			},
			deferred: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			memory := storage.NewMemoryStore()
			path := writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript)
			if err := local.SaveRegistration(registration("s1", path)); err != nil {
				t.Fatal(err)
			}
			if result := collect(t, local, memory, t0); len(result.Published) != 1 {
				t.Fatalf("not published: %#v", result)
			}
			if tc.after != nil {
				tc.after(t, local, path)
			}
			result := sweep(t, local, memory, expiry, Options{})
			if len(result.Errors) != 0 {
				t.Fatalf("errors: %#v", result.Errors)
			}
			expired := len(result.DeletedSessions) == 1 && registered(t, local) == 0
			if expired == tc.deferred {
				t.Fatalf("deferred = %v, want %v (result %#v)", !expired, tc.deferred, result)
			}
		})
	}
}
