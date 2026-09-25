package storagetest

import (
	"context"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// MemoryStore stands in for S3 and R2 in reader tests, so its ETag must
// behave like theirs for a single-part object: the bare MD5 of the bytes.
func TestMemoryStoreListsAnMD5ETag(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.Put(ctx, "sessions/a/metadata.json", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	items, err := store.List(ctx, "sessions/")
	if err != nil || len(items) != 1 || items[0].ETag != md5Hex([]byte(`{"v":1}`)) {
		t.Fatalf("List = %#v, %v", items, err)
	}
	if err := store.Put(ctx, "sessions/a/metadata.json", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	after, _ := store.List(ctx, "sessions/")
	if after[0].ETag == items[0].ETag || after[0].ETag != md5Hex([]byte(`{"v":2}`)) {
		t.Fatalf("ETag did not follow the content: %q then %q", items[0].ETag, after[0].ETag)
	}
}
