package state

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func testBundle(id string) archive.SourceBundle {
	return archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "session-1", NativeSessionID: id}
}

// A collector scan loads a session's Published once and reads it again after
// each change it makes, so every save must leave the in-memory copy exactly
// what the file now holds. Each step checks the same Published it saved
// through, then a fresh load, against the same expectations.
func TestPublishedReflectsEverySaveInMemory(t *testing.T) {
	store := newTestStore(t)
	p, err := store.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	source := archive.SourceReference{Key: "sessions/codex/session-1/source.a.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 10}
	republished := archive.SourceReference{Key: "sessions/codex/session-1/source.b.jsonl.gz", SHA256: strings.Repeat("b", 64), CompressedBytes: 11}

	type want struct {
		cached, last string
		status       CacheStatus
		found        bool
		lastFound    bool
		source       *archive.SourceReference
		metadata     string
		blocked      BlockedReason
	}
	check := func(step string, w want) {
		t.Helper()
		reloaded, err := store.LoadPublishedState("session-1")
		if err != nil {
			t.Fatal(err)
		}
		for _, view := range []struct {
			name string
			p    *Published
		}{{"in memory", p}, {"reloaded", reloaded}} {
			bundle, _, status, found := view.p.Cached()
			if found != w.found || status != w.status || (found && bundle.NativeSessionID != w.cached) {
				t.Fatalf("%s, %s: Cached = %q %q %v, want %q %q %v", step, view.name, bundle.NativeSessionID, status, found, w.cached, w.status, w.found)
			}
			last, _, lastFound := view.p.LastPublished()
			if lastFound != w.lastFound || (lastFound && last.NativeSessionID != w.last) {
				t.Fatalf("%s, %s: LastPublished = %q %v, want %q %v", step, view.name, last.NativeSessionID, lastFound, w.last, w.lastFound)
			}
			got, gotFound := view.p.LastPublishedSource()
			if (w.source == nil) == gotFound || (w.source != nil && got != *w.source) {
				t.Fatalf("%s, %s: LastPublishedSource = %#v %v, want %#v", step, view.name, got, gotFound, w.source)
			}
			if string(view.p.Metadata()) != w.metadata {
				t.Fatalf("%s, %s: Metadata = %q, want %q", step, view.name, view.p.Metadata(), w.metadata)
			}
			if reason, _ := view.p.Blocked(); reason != w.blocked {
				t.Fatalf("%s, %s: Blocked = %q, want %q", step, view.name, reason, w.blocked)
			}
		}
	}

	check("nothing saved", want{})
	must(t, p.Save(testBundle("rate-limited"), t0, CacheStatusRateLimited))
	check("Save", want{cached: "rate-limited", status: CacheStatusRateLimited, found: true})
	must(t, p.SavePublication(testBundle("published"), t0.Add(time.Hour), source, []byte(`{"m":1}`)))
	check("SavePublication", want{cached: "published", last: "published", status: CacheStatusPublished, found: true, lastFound: true, source: &source, metadata: `{"m":1}`})
	must(t, p.Save(testBundle("declined"), t0.Add(2*time.Hour), CacheStatusDeclined))
	check("Save over a publication", want{cached: "declined", last: "published", status: CacheStatusDeclined, found: true, lastFound: true, source: &source, metadata: `{"m":1}`})
	must(t, p.CacheMetadata([]byte(`{"m":2}`)))
	check("CacheMetadata", want{cached: "declined", last: "published", status: CacheStatusDeclined, found: true, lastFound: true, source: &source, metadata: `{"m":2}`})
	must(t, p.SaveRepublishedMetadata(PendingPublication{Bundle: testBundle("republished"), MetadataBytes: []byte(`{"m":3}`), SourceKey: republished.Key, SourceSHA256: republished.SHA256, SourceBytes: make([]byte, republished.CompressedBytes)}, t0.Add(3*time.Hour)))
	check("SaveRepublishedMetadata", want{cached: "declined", last: "republished", status: CacheStatusDeclined, found: true, lastFound: true, source: &republished, metadata: `{"m":3}`})
	must(t, p.SaveBlocked(testBundle("blocked"), t0.Add(3*time.Hour), BlockedReasonTranscriptMissing))
	check("SaveBlocked", want{cached: "blocked", last: "republished", status: CacheStatusBlocked, found: true, lastFound: true, source: &republished, metadata: `{"m":3}`, blocked: BlockedReasonTranscriptMissing})
	restored, _, cleared, err := p.ClearRecoverableBlock(t0.Add(4 * time.Hour))
	if err != nil || !cleared || restored != CacheStatusDeclined {
		t.Fatalf("ClearRecoverableBlock = %q %v %v", restored, cleared, err)
	}
	check("ClearRecoverableBlock", want{cached: "blocked", last: "republished", status: CacheStatusDeclined, found: true, lastFound: true, source: &republished, metadata: `{"m":3}`})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
