package retention

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
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

// TestExpiryDeferralMatchesTheRuleBeforeOutstanding sweeps every
// combination of what a session can owe, how it was captured, and the
// options that switch deferral off, and checks each against the rule
// retention had before state.Outstanding existed: an expired session is
// kept only when the collector would do its work (it is publishable, and
// is not a file session that never received a transcript path and whose
// admission is itself past retention) and that work is a queued request or
// a publication storage has not accepted. Nothing else a session owes, and
// nothing it is (blocked, declined, rate limited, never captured, in
// another destination), changes the verdict.
func TestExpiryDeferralMatchesTheRuleBeforeOutstanding(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	expiry := t0.Add(retentionWindow + 24*time.Hour)
	type source int
	const (
		withPath source = iota
		withoutPath
		cursorDatabase
	)
	captures := map[source][]state.CacheStatus{
		// "" is never captured.
		withPath:       {"", state.CacheStatusPublished, state.CacheStatusRateLimited, state.CacheStatusBlocked, state.CacheStatusDeclined},
		withoutPath:    {""},
		cursorDatabase: {""},
	}
	for _, src := range []source{withPath, withoutPath, cursorDatabase} {
		for _, capture := range captures[src] {
			for bits := range 32 {
				requested, scan, upload := bits&1 != 0, bits&2 != 0, bits&4 != 0
				publishable, current := bits&8 == 0, bits&16 == 0
				name := fmt.Sprintf("source %d capture %q request %v scan %v upload %v publishable %v current %v",
					src, capture, requested, scan, upload, publishable, current)
				t.Run(name, func(t *testing.T) {
					local := newTestStore(t)
					reg := registration("s1", "")
					switch src {
					case withPath:
						reg.TranscriptPath = writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript)
					case withoutPath:
					case cursorDatabase:
						reg.Harness.Name = "cursor"
						reg.SourceKind, reg.SourceKey = archive.SourceKindCursorSQLite, reg.NativeSessionID
					}
					if err := local.SaveRegistration(reg); err != nil {
						t.Fatal(err)
					}
					bundle := archive.SourceBundle{Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: t0}}
					switch capture {
					case "":
					case state.CacheStatusPublished, state.CacheStatusRateLimited:
						if result := collect(t, local, storage.NewMemoryStore(), t0); len(result.Published) != 1 {
							t.Fatalf("not published: %#v", result)
						}
						published, err := local.LoadPublishedState("s1")
						if err != nil {
							t.Fatal(err)
						}
						bundle, _, _ = published.LastPublished()
						if capture == state.CacheStatusRateLimited {
							// A rate-limited candidate whose pending file is
							// gone; the upload facet says whether one is there.
							if err := published.Save(bundle, t0, capture); err != nil {
								t.Fatal(err)
							}
						}
					case state.CacheStatusBlocked, state.CacheStatusDeclined:
						published, err := local.LoadPublishedState("s1")
						if err != nil {
							t.Fatal(err)
						}
						if err := published.Save(bundle, t0, capture); err != nil {
							t.Fatal(err)
						}
					}
					if requested {
						if err := local.SaveRequest("s1", "stop", t0.Add(time.Minute)); err != nil {
							t.Fatal(err)
						}
					}
					if scan {
						if err := local.SetScanPending("s1", true); err != nil {
							t.Fatal(err)
						}
					}
					if upload {
						if err := local.SavePending("s1", state.PendingPublication{
							Bundle: bundle, SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1},
						}); err != nil {
							t.Fatal(err)
						}
					}

					result := sweep(t, local, storage.NewMemoryStore(), expiry, Options{
						Publishable:        func(archive.SessionRegistration) bool { return publishable },
						CurrentDestination: func(archive.SessionRegistration) bool { return current },
					})
					if len(result.Errors) != 0 {
						t.Fatalf("errors: %#v", result.Errors)
					}
					// The rule before Outstanding (main's deferForWork and
					// hasUnfinishedWork in retention.go).
					deferForWork := publishable && src != withoutPath
					want := deferForWork && (requested || upload)
					if kept := registered(t, local) == 1; kept != want {
						t.Fatalf("kept = %v, want %v (result %#v)", kept, want, result)
					}
				})
			}
		}
	}
}
