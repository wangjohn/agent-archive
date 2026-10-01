package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func sample(t *testing.T, id string) Record {
	t.Helper()
	r, e := Build(config.Config{MachineID: id, Storage: credentials.Config{Provider: credentials.ProviderS3}}, "darwin/arm64", "dev", "", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestListRejectsUntrustedRecordsWithoutLosingValidOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	id := strings.Repeat("a", 32)
	valid := sample(t, id)
	if e := Publish(ctx, store, valid); e != nil {
		t.Fatal(e)
	}
	for i, change := range []func(*Record){func(r *Record) { r.SchemaVersion = 2 }, func(r *Record) { r.MachineID = id }, func(r *Record) { r.Platform = "darwin\x1b[2J" }, func(r *Record) { r.Name = "bad\nname" }, func(r *Record) { r.Credential.Kind = "future" }, func(r *Record) { r.Credential.Kind = "r2_own" }} {
		bad := sample(t, fmt.Sprintf("%032x", i+1))
		change(&bad)
		b, _ := json.Marshal(bad)
		if e := store.Put(ctx, fmt.Sprintf("machines/%032x.json", i+1), b); e != nil {
			t.Fatal(e)
		}
	}
	_ = store.Put(ctx, "machines/../../forged\x1b.json", []byte(`{}`))
	_ = store.Put(ctx, "machines/"+strings.Repeat("b", 32)+".json", []byte(strings.Repeat("x", MaxRecordBytes+1)))
	got := List(ctx, store)
	if len(got.Records) != 1 || len(got.Unreadable) != 8 || got.ProviderVerified {
		t.Fatalf("%+v", got)
	}
	for _, u := range got.Unreadable {
		if strings.ContainsAny(u.Key, "\x1b\n\r") {
			t.Fatalf("unsafe diagnostic: %q", u.Key)
		}
	}
}

type boundedStore struct {
	*storagetest.MemoryStore
	active, maximum atomic.Int32
	reads           atomic.Int32
	repeat          bool
	deadline        atomic.Bool
}

func (s *boundedStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.reads.Add(1)
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		old := s.maximum.Load()
		if n <= old || s.maximum.CompareAndSwap(old, n) {
			break
		}
	}
	if d, ok := ctx.Deadline(); ok && time.Until(d) <= Timeout {
		s.deadline.Store(true)
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}
func (s *boundedStore) ListPage(ctx context.Context, prefix, token string, limit int32) (storage.ObjectPage, error) {
	if prefix != "machines/" {
		return storage.ObjectPage{}, errors.New("session scan")
	}
	p, e := s.MemoryStore.ListPage(ctx, prefix, token, limit)
	if s.repeat {
		p.Next = "repeat"
	}
	return p, e
}

func TestListCapsPaginationAndSharesDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &boundedStore{MemoryStore: storagetest.NewMemoryStore()}
	for i := range 1001 {
		if e := Publish(ctx, s, sample(t, fmt.Sprintf("%032x", i))); e != nil {
			t.Fatal(e)
		}
	}
	got := List(ctx, s)
	if !got.Partial || len(got.Records) != 1000 || s.reads.Load() != 1000 || s.maximum.Load() > 4 || !s.deadline.Load() {
		t.Fatalf("%d records partial=%v reads=%d concurrency=%d deadline=%v", len(got.Records), got.Partial, s.reads.Load(), s.maximum.Load(), s.deadline.Load())
	}
	s.repeat = true
	got = List(ctx, s)
	if !got.Partial || len(got.Records) > 200 {
		t.Fatal("repeated token was not stopped")
	}
}

func TestBuildPreservesLocalProvenanceOnlyAtMatchingDestination(t *testing.T) {
	t.Parallel()
	cfg := config.Config{MachineID: strings.Repeat("a", 32), Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}}
	r, e := Build(cfg, "linux/amd64", "dev", "KEY", time.Now())
	if e != nil || r.Credential.Kind != "r2_unknown" {
		t.Fatalf("%+v %v", r, e)
	}
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: "r2_shared", AccessKeyID: "KEY", SharedWith: strings.Repeat("b", 32), RecipientID: strings.Repeat("c", 32)}
	r, e = Build(cfg, "linux/amd64", "dev", "KEY", time.Now())
	if e != nil || r.Credential.Kind != "r2_shared" {
		t.Fatalf("%+v %v", r, e)
	}
	cfg.Storage.Bucket = "other"
	r, e = Build(cfg, "linux/amd64", "dev", "NEW", time.Now())
	if e != nil || r.Credential.Kind != "r2_unknown" || r.Credential.AccessKeyID != "NEW" {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestSelectRequiresIDForDuplicateNames(t *testing.T) {
	t.Parallel()
	a, b := sample(t, strings.Repeat("a", 32)), sample(t, strings.Repeat("b", 32))
	a.Name = "laptop"
	b.Name = "laptop"
	if _, e := Select([]Record{a, b}, "laptop"); e == nil {
		t.Fatal("ambiguous name accepted")
	}
	r, e := Select([]Record{a, b}, b.MachineID)
	if e != nil || r.MachineID != b.MachineID {
		t.Fatal(e)
	}
}

type failingPageStore struct{ *storagetest.MemoryStore }

func (s failingPageStore) ListPage(ctx context.Context, prefix, token string, limit int32) (storage.ObjectPage, error) {
	if token != "" {
		return storage.ObjectPage{}, errors.New("private service details")
	}
	return s.MemoryStore.ListPage(ctx, prefix, token, 1)
}

func TestListKeepsEarlierRecordsAfterLaterPageFails(t *testing.T) {
	t.Parallel()
	s := failingPageStore{storagetest.NewMemoryStore()}
	for i := range 2 {
		if e := Publish(context.Background(), s, sample(t, fmt.Sprintf("%032x", i))); e != nil {
			t.Fatal(e)
		}
	}
	got := List(context.Background(), s)
	if !got.Partial || len(got.Records) != 1 || len(got.Unreadable) != 1 || strings.Contains(got.Unreadable[0].Reason, "private") {
		t.Fatalf("%+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	got = List(ctx, s)
	if !got.Partial || time.Since(start) > time.Second {
		t.Fatal("cancelled listing did not stop")
	}
}
