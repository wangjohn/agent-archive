package storagetest

import (
	"context"
	"reflect"
	"testing"
)

func TestMemoryStoreListPageContinuesInKeyOrder(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	for _, key := range []string{"listing/a", "listing/b", "listing/c", "other/d"} {
		if err := store.Put(ctx, key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ListPage(ctx, "listing/", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ListPage(ctx, "listing/", first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, object := range append(first.Objects, second.Objects...) {
		got = append(got, object.Key)
	}
	if !reflect.DeepEqual(got, []string{"listing/a", "listing/b", "listing/c"}) || first.Next == "" || second.Next != "" {
		t.Fatalf("pages: first=%+v second=%+v", first, second)
	}
}
