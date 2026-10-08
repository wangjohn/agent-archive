package reader

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"testing"
	"time"
)

func TestSelectMetadataUnlimitedDateSelectionMatchesExhaustive(t *testing.T) {
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for i := range 400 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*24*time.Hour))
	}
	if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []Filter{
		{From: baseTime.Add(365 * 24 * time.Hour)},
		{From: baseTime.Add(390 * 24 * time.Hour), To: baseTime.Add(395 * 24 * time.Hour)},
		{From: baseTime.Add(401 * 24 * time.Hour)},
		{Model: "no-such-model"},
	} {
		oracle, err := ListMetadata(t.Context(), store, "sessions", filter)
		if err != nil {
			t.Fatal(err)
		}
		store.reset()
		got, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: filter}, ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Sessions, oracle) {
			t.Fatalf("filter %+v: got %d want %d", filter, len(got.Sessions), len(oracle))
		}
		_, gets := store.counts()
		want := len(oracle)
		if filter.Model != "" {
			want = 400
		}
		if len(gets) != want {
			t.Fatalf("filter %+v: GETs %d want %d", filter, len(gets), want)
		}
	}
}

func TestFindMetadataPrefixUsesFreshCanonicalHeadersAndCandidateBodies(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	first := putSession(t, store, "codex", "abcdef01", baseTime)
	putSession(t, store, "claude", "abcdef02", baseTime)
	putSession(t, store, "codex", "other", baseTime)
	for _, includeText := range []bool{false, true} {
		store.reset()
		var matchers []func(archive.Metadata) bool
		if includeText {
			matchers = append(matchers, func(m archive.Metadata) bool { return m.SessionID == "other" })
		}
		got, err := FindMetadataPrefix(ctx, store, "sessions", "abcdef", Filter{}, ListOptions{}, matchers...)
		if err != nil {
			t.Fatal(err)
		}
		lists, gets := store.counts()
		want := 2
		if includeText {
			want = 3
		}
		if len(got) != want || len(lists) != 1 || len(gets) != want {
			t.Fatalf("matches=%d lists=%d GETs=%d want=%d", len(got), len(lists), len(gets), want)
		}
	}
	if err := store.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := FindMetadataPrefix(ctx, store, "sessions", "abcdef", Filter{}, ListOptions{})
	if err != nil || len(got) != 1 || got[0].Harness.Name != "claude" {
		t.Fatalf("deleted prefix result=%+v err=%v", got, err)
	}
}
