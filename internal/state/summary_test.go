package state

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func summaryBundle(capturedAt time.Time, links ...archive.LinkedSessionReference) archive.SourceBundle {
	return archive.SourceBundle{
		ArchiveSessionID: "session-1",
		Capture:          archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, CapturedAt: capturedAt},
		LinkedSessions:   links,
	}
}

// The summary read from the head of the file says exactly what a full decode
// does, through every kind of save.
func TestPublishedSummaryMatchesTheFullState(t *testing.T) {
	store := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	check := func(label string) {
		t.Helper()
		head, found, err := store.LoadPublishedSummary("session-1")
		if err != nil || !found {
			t.Fatalf("%s: %v %v", label, found, err)
		}
		p, err := store.LoadPublishedState("session-1")
		if err != nil {
			t.Fatal(err)
		}
		if full := p.Summary(); !reflect.DeepEqual(head, full) {
			t.Fatalf("%s: head %#v, full %#v", label, head, full)
		}
	}
	p, err := store.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadPublishedSummary("session-1"); found || err != nil {
		t.Fatalf("summary of nothing: %v %v", found, err)
	}
	link := archive.LinkedSessionReference{SessionID: "child", Status: archive.LinkedSessionPublished}
	ref := archive.SourceReference{Key: "sessions/codex/session-1/source.a.jsonl.gz", SHA256: "0000000000000000000000000000000000000000000000000000000000000000", CompressedBytes: 1}
	if err := p.SavePublication(summaryBundle(at, link), at, ref, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	check("published")
	if head, _, _ := store.LoadPublishedSummary("session-1"); !head.Published || !head.LinksPublished("child") || !head.CapturedAt.Equal(at) {
		t.Fatalf("published summary = %#v", head)
	}
	if err := p.Save(summaryBundle(at.Add(time.Hour)), at, CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	check("rate limited")
	if err := p.SaveBlocked(summaryBundle(at.Add(time.Hour)), at, BlockedReasonTranscriptMissing); err != nil {
		t.Fatal(err)
	}
	check("blocked")
	if head, _, _ := store.LoadPublishedSummary("session-1"); head.Status != CacheStatusBlocked || head.BlockedReason != BlockedReasonTranscriptMissing || !head.Published {
		t.Fatalf("blocked summary = %#v", head)
	}
}

// State written before the summary existed has none at its head; it is
// decoded in full instead, with the same answer.
func TestPublishedSummaryOfOlderStateDecodesInFull(t *testing.T) {
	store := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	legacy := publishedState{Bundle: summaryBundle(at), PublishedAt: at, Status: CacheStatusPublished}
	if err := local.Write(store.publishedPath("session-1"), legacy); err != nil {
		t.Fatal(err)
	}
	head, found, err := store.LoadPublishedSummary("session-1")
	if err != nil || !found || !head.Published || !head.CapturedAt.Equal(at) || head.Status != CacheStatusPublished {
		t.Fatalf("%#v %v %v", head, found, err)
	}
	if _, ok := readLeadingSummary(store.publishedPath("session-1")); ok {
		t.Fatal("a reader outside a collector pass rewrote the file")
	}
	// A collector pass, the file's writer, migrates it once, so later reads
	// need not decode it in full.
	if _, _, err := store.ForCollectorPass().LoadPublishedSummary("session-1"); err != nil {
		t.Fatal(err)
	}
	if migrated, ok := readLeadingSummary(store.publishedPath("session-1")); !ok || !reflect.DeepEqual(migrated, head) {
		t.Fatalf("not migrated: %#v %v", migrated, ok)
	}
}

// A clamp belongs to the capture it was recorded for: a later capture drops
// it, and only an earlier time than the one in force is recorded.
func TestAgeClampBelongsToOneCapture(t *testing.T) {
	store := newTestStore(t)
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, err := store.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(summaryBundle(future), time.Time{}, CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	if err := p.ClampAgeFrom(now); err != nil {
		t.Fatal(err)
	}
	if err := p.ClampAgeFrom(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if head, _, _ := store.LoadPublishedSummary("session-1"); !head.RetentionAge().Equal(now) {
		t.Fatalf("retention age = %v, want %v", head.RetentionAge(), now)
	}
	// The same capture saved again (a block, say) keeps its clamp.
	if err := p.SaveBlocked(summaryBundle(future), time.Time{}, BlockedReasonTranscriptMissing); err != nil {
		t.Fatal(err)
	}
	if head, _, _ := store.LoadPublishedSummary("session-1"); !head.RetentionAge().Equal(now) {
		t.Fatalf("a re-save of the same capture lost its clamp: %v", head.RetentionAge())
	}
	later := now.Add(24 * time.Hour)
	if err := p.Save(summaryBundle(later), time.Time{}, CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	if head, _, _ := store.LoadPublishedSummary("session-1"); !head.RetentionAge().Equal(later) || !head.AgeFrom.IsZero() {
		t.Fatalf("a new capture kept the old clamp: %#v", head)
	}
}

// ClampSuperseded moves only future entries, and keeps the ledger's order.
func TestClampSupersededKeepsOrder(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{now.Add(-time.Hour), now.Add(48 * time.Hour), now.Add(24 * time.Hour)} {
		if err := store.RecordSuperseded("session-1", string(rune('a'+i)), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ClampSuperseded("session-1", now); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []SupersededSource{{Key: "a", SupersededAt: now.Add(-time.Hour)}, {Key: "b", SupersededAt: now}, {Key: "c", SupersededAt: now}}
	if !reflect.DeepEqual(ledger, want) {
		t.Fatalf("ledger = %#v", ledger)
	}
	if _, err := os.Stat(store.supersededPath("session-1")); err != nil {
		t.Fatal(err)
	}
}
