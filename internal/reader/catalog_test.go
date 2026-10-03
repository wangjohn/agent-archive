package reader

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"testing"
)

const testCatalogID agentmeta.ID = "synthetic"

func TestInjectedCatalogProbesAndUnknownFallback(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testCatalogID, DisplayName: "Synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	finder := NewMetadataFinder(c)
	store := newCountingStore()
	known := putSession(t, store, "synthetic", "known", baseTime)
	keys, err := finder.FindMetadataKeys(context.Background(), store, "sessions", "known")
	if err != nil || len(keys) != 1 || keys[0] != known {
		t.Fatalf("keys %q err %v", keys, err)
	}
	lists, gets := store.counts()
	if len(lists) != 0 || len(gets) != 1 {
		t.Fatalf("probes lists=%q gets=%q", lists, gets)
	}
	unknown := putSession(t, store, "future-agent", "unknown", baseTime)
	keys, err = finder.FindMetadataKeys(context.Background(), store, "sessions", "unknown")
	if err != nil || len(keys) != 1 || keys[0] != unknown {
		t.Fatalf("unknown keys %q err %v", keys, err)
	}
	metas, err := ListMetadata(context.Background(), store, "sessions", Filter{})
	if err != nil || len(metas) != 2 {
		t.Fatalf("listing hid unknown agent: %d %v", len(metas), err)
	}
	empty, err := agentmeta.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMetadataFinder(empty).FindMetadataKeys(context.Background(), store, "sessions", "known"); err != nil {
		t.Fatal(err)
	}
}
