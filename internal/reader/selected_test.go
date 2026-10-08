package reader

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type selectedGetterFunc func(context.Context, string) ([]byte, string, error)

func (f selectedGetterFunc) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	return f(ctx, key)
}

func selectedFixture(t *testing.T, count int) (*storagetest.MemoryStore, []listingindex.Revision) {
	t.Helper()
	store := storagetest.NewMemoryStore()
	revisions := make([]listingindex.Revision, count)
	for i := range count {
		key := putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
		data, etag, err := store.GetVersioned(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		revisions[i], err = listingindex.NewRevision(key, data, etag)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, revisions
}

func TestSelectedReadsOverlapAndWarmCacheAvoidsGETs(t *testing.T) {
	store, revisions := selectedFixture(t, 50)
	measured := storagetest.NewMeasuredStore(store, time.Millisecond)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	opts := ListOptions{Cache: cache, BodyRead: func(key string, _ bool) { observed = append(observed, key) }}
	reads := readSelected(context.Background(), measured, revisions, opts)
	want := make([]string, len(revisions))
	for i, r := range revisions {
		want[i] = r.MetadataKey
		if reads[i].Err != nil || reads[i].Cached {
			t.Fatalf("cold slot %d: %+v", i, reads[i])
		}
	}
	if !reflect.DeepEqual(observed, want) {
		t.Fatal("observer order differs from selection")
	}
	metrics := measured.Metrics()
	if metrics.Gets != 50 || metrics.PeakReads < 2 || metrics.PeakReads > 8 {
		t.Fatalf("cold reads: %+v", metrics)
	}
	reads = readSelected(context.Background(), measured, revisions, opts)
	for i, read := range reads {
		if read.Err != nil || !read.Cached {
			t.Fatalf("warm slot %d: %+v", i, read)
		}
	}
	if measured.Metrics().Gets != 50 {
		t.Fatal("warm cache made remote GETs")
	}
}

func TestSelectedFailureStopsDispatchAndJoinsEarlierErrors(t *testing.T) {
	_, revisions := selectedFixture(t, 50)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int64
		earlier, later := errors.New("earlier revision failure"), errors.New("later revision failure")
		getter := selectedGetterFunc(func(_ context.Context, key string) ([]byte, string, error) {
			calls.Add(1)
			if key == revisions[1].MetadataKey {
				return nil, "", later
			}
			<-release
			return nil, "", earlier
		})
		done := make(chan []selectedRead)
		go func() { done <- readSelected(context.Background(), getter, revisions, ListOptions{}) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("returned before earlier in-flight reads joined")
		default:
		}
		if calls.Load() > 8 {
			t.Fatalf("dispatched after failure: %d", calls.Load())
		}
		close(release)
		reads := <-done
		if len(reads) < 2 || !errors.Is(reads[0].Err, earlier) || !errors.Is(reads[1].Err, later) {
			t.Fatalf("errors lost selection order: %+v", reads)
		}
	})
}

func TestSelectedCancellationStopsDispatchAndJoins(t *testing.T) {
	_, revisions := selectedFixture(t, 50)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int64
		getter := selectedGetterFunc(func(ctx context.Context, _ string) ([]byte, string, error) {
			calls.Add(1)
			<-ctx.Done()
			return nil, "", ctx.Err()
		})
		done := make(chan []selectedRead)
		go func() { done <- readSelected(ctx, getter, revisions, ListOptions{}) }()
		synctest.Wait()
		if calls.Load() != 8 {
			t.Fatalf("active reads=%d", calls.Load())
		}
		cancel()
		reads := <-done
		if len(reads) != 8 || calls.Load() != 8 {
			t.Fatalf("dispatched after cancellation: slots=%d calls=%d", len(reads), calls.Load())
		}
		for _, read := range reads {
			if !errors.Is(read.Err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", read.Err)
			}
		}
		if reads := readSelected(ctx, getter, revisions, ListOptions{}); len(reads) != 0 {
			t.Fatal("canceled query dispatched reads")
		}
	})
}

func TestSelectedReadsRejectInvalidRevisionEvidence(t *testing.T) {
	store, original := selectedFixture(t, 1)
	data, etag, err := store.GetVersioned(context.Background(), original[0].MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing", "validator", "hash", "summary", "schema"} {
		t.Run(kind, func(t *testing.T) {
			revisions := append([]listingindex.Revision(nil), original...)
			response, validator := data, etag
			var responseErr error
			switch kind {
			case "missing":
				responseErr = errors.New("missing selected body")
			case "validator":
				validator = "changed"
			case "hash":
				response = []byte("corrupt")
			case "summary":
				revisions[0].ProjectID = "wrong-project"
			case "schema":
				response = []byte(`{"schema_version":999}`)
				revisions[0].Hash = storage.SHA256Hex(response)
			}
			getter := selectedGetterFunc(func(context.Context, string) ([]byte, string, error) { return response, validator, responseErr })
			reads := readSelected(context.Background(), getter, revisions, ListOptions{})
			if len(reads) != 1 || reads[0].Err == nil {
				t.Fatalf("accepted %s evidence: %+v", kind, reads)
			}
		})
	}
}
