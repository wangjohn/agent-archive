package cli

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestStatsIndexedFetchEqualsExhaustiveAcrossShapesAndWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, shape := range statsShapes() {
		t.Run(shape.name, func(t *testing.T) {
			store := storagetest.NewMemoryStore()
			for _, s := range shape.build(now, time.UTC) {
				s.publish(t, store)
			}
			if _, err := reader.RebuildIndex(ctx, store, "sessions"); err != nil {
				t.Fatal(err)
			}
			windows, _ := statsWindowCycle(200)
			filter := reader.Filter{From: statsFetchFrom(now, time.UTC, slices.Max(windows))}
			exhaustive, err := reader.ListMetadataWithOptions(ctx, store, "sessions", filter, reader.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			indexed, err := reader.SelectMetadata(ctx, store, "sessions", reader.MetadataQuery{Filter: filter}, reader.ListOptions{CompatibilityScan: func(reason string) { t.Fatalf("unexpected fallback: %s", reason) }})
			if err != nil {
				t.Fatal(err)
			}
			for _, days := range windows {
				got, err := json.Marshal(statsInputs{sessions: indexed.Sessions, now: now, location: time.UTC}.compute(days, true))
				if err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(statsInputs{sessions: exhaustive, now: now, location: time.UTC}.compute(days, true))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Fatalf("window %d indexed stats differ from exhaustive", days)
				}
			}
		})
	}
}
