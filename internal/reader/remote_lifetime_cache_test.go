package reader

import (
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/catalog"
)

func TestRemoteSQLContinuationRejectsChangedHeadWithoutLocalRefresh(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 8)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	q := CatalogQuery{Words: []string{"résumé"}, Metadata: MetadataQuery{Limit: 1}}
	page, err := c.Query(t.Context(), q)
	if err != nil || page.Next == "" || len(page.Rows) != 1 {
		t.Fatal("first SQL page", page, err)
	}
	if err = remote.DeleteSession(t.Context(), page.Rows[0].Key); err != nil {
		t.Fatal(err)
	}
	q.Cursor = page.Next
	if _, err = c.Query(t.Context(), q); !errors.Is(err, catalog.ErrStaleCursor) && !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatal("SQL continuation accepted changed remote root", err)
	}
}

func TestRemoteRefreshEvictsOnlyProvenDeletedBodyCache(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "delta", true: "rebuild"}[rebuild], func(t *testing.T) {
			remote, legacy := remoteReaderFixture(t, 8)
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			page, err := c.Query(t.Context(), CatalogQuery{})
			if err != nil || len(page.Rows) < 2 {
				t.Fatal(err)
			}
			deleted, live := page.Rows[0], page.Rows[1]
			for _, row := range []CatalogRow{deleted, live} {
				raw, e := legacy.Get(t.Context(), row.Key)
				if e != nil {
					t.Fatal(e)
				}
				cache.putVerified(row.Key, row.ETag, raw)
			}
			goneDir, ok := cache.keyDir(deleted.Key)
			if !ok {
				t.Fatal("uncacheable fixture")
			}
			liveDir, _ := cache.keyDir(live.Key)
			if err = remote.DeleteSession(t.Context(), deleted.Key); err != nil {
				t.Fatal(err)
			}
			if rebuild {
				if _, err = c.db.ExecContext(t.Context(), "DELETE FROM remote_root"); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.RefreshRemote(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(goneDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("verified deletion retained body directory", err)
			}
			if _, err = os.Stat(liveDir); err != nil {
				t.Fatal("live body evicted", err)
			}
		})
	}
}
