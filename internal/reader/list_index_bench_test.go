package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func BenchmarkListLargeArchive(b *testing.B) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	for i := range 10000 {
		id := fmt.Sprintf("%032x", i+1)
		metadataKey, _ := archive.MetadataObjectKey("codex", id)
		sourceKey := fmt.Sprintf("sessions/codex/%s/source.%s.jsonl.gz", id, fakeSourceSHA)
		m := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: id, MachineID: "bench", ProjectID: "bench", Harness: archive.Harness{Name: "codex"}, CapturedAt: baseTime.Add(time.Duration(i) * time.Second), SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: fakeSourceSHA, CompressedBytes: 1}}
		data, _ := json.Marshal(m)
		if err := store.Put(ctx, metadataKey, data); err != nil {
			b.Fatal(err)
		}
		for j := range 3 {
			key := fmt.Sprintf("sessions/codex/%s/source.%064x.jsonl.gz", id, i*3+j+1)
			if err := store.Put(ctx, key, []byte("snapshot")); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("baseline-cold", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := ListMetadataWithOptions(ctx, store, "sessions", Filter{}, ListOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		b.Fatal(err)
	}
	b.Run("indexed-cold", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	cache, err := OpenMetadataCache(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if _, err = ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{Cache: cache}); err != nil {
		b.Fatal(err)
	}
	b.Run("indexed-warm", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{Cache: cache}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
