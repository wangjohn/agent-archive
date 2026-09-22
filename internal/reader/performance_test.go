package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// countingStore records every List prefix and Get key, the most Gets it saw
// in flight at once, and can fail or slow chosen reads.
type countingStore struct {
	*storage.MemoryStore
	delay   time.Duration
	failGet map[string]error

	mu          sync.Mutex
	lists       []string
	gets        []string
	inFlight    int
	maxInFlight int
}

func newCountingStore() *countingStore {
	return &countingStore{MemoryStore: storage.NewMemoryStore(), failGet: map[string]error{}}
}

func (s *countingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	s.lists = append(s.lists, prefix)
	s.mu.Unlock()
	return s.MemoryStore.List(ctx, prefix)
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.gets = append(s.gets, key)
	s.inFlight++
	if s.inFlight > s.maxInFlight {
		s.maxInFlight = s.inFlight
	}
	err := s.failGet[key]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if err != nil {
		return nil, err
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *countingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists, s.gets, s.maxInFlight = nil, nil, 0
}

func (s *countingStore) counts() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lists...), append([]string(nil), s.gets...)
}

var fakeSourceSHA = strings.Repeat("b", 64)

// putSession publishes one metadata sidecar and a source object beside it, so
// a listing always contains keys which are not sidecars.
func putSession(t *testing.T, store storage.ObjectStore, harness, id string, captured time.Time) string {
	t.Helper()
	metadataKey, err := archive.MetadataObjectKey(harness, id)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := "sessions/" + harness + "/" + id + "/source." + fakeSourceSHA + ".json.gz"
	metadata := archive.Metadata{
		SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: "native-" + id,
		MachineID: "machine", ProjectID: "project", Harness: archive.Harness{Name: harness}, CapturedAt: captured,
		SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: fakeSourceSHA, CompressedBytes: 1},
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, metadataKey, data); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, sourceKey, []byte("source bytes, never read by list")); err != nil {
		t.Fatal(err)
	}
	return metadataKey
}

var baseTime = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func TestListWithHarnessListsOnlyThatHarnessPrefix(t *testing.T) {
	store := newCountingStore()
	putSession(t, store, "codex", "c1", baseTime)
	putSession(t, store, "codex", "c2", baseTime.Add(time.Hour))
	putSession(t, store, "claude", "a1", baseTime.Add(2*time.Hour))

	results, err := ListMetadata(context.Background(), store, "sessions", Filter{Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	lists, gets := store.counts()
	if len(lists) != 1 || lists[0] != "sessions/codex/" {
		t.Fatalf("listed prefixes = %q, want only sessions/codex/", lists)
	}
	for _, key := range gets {
		if !strings.HasPrefix(key, "sessions/codex/") || !strings.HasSuffix(key, "/metadata.json") {
			t.Errorf("read %q outside the harness's sidecars", key)
		}
	}
	if len(gets) != 2 || len(results) != 2 || results[0].SessionID != "c2" || results[1].SessionID != "c1" {
		t.Fatalf("gets=%q results=%#v", gets, results)
	}
}

func TestListSkipsNonMetadataKeysBeforeAnyGet(t *testing.T) {
	store := newCountingStore()
	for i := 0; i < 3; i++ {
		putSession(t, store, "codex", fmt.Sprintf("s%d", i), baseTime.Add(time.Duration(i)*time.Minute))
	}
	if _, err := ListMetadata(context.Background(), store, "sessions", Filter{}); err != nil {
		t.Fatal(err)
	}
	_, gets := store.counts()
	if len(gets) != 3 {
		t.Fatalf("gets = %q, want the three sidecars only", gets)
	}
	for _, key := range gets {
		if !strings.HasSuffix(key, "/metadata.json") {
			t.Fatalf("list downloaded a non-metadata object %q", key)
		}
	}
}

func TestListReadsSidecarsWithBoundedConcurrency(t *testing.T) {
	store := newCountingStore()
	for i := 0; i < 40; i++ {
		putSession(t, store, "codex", fmt.Sprintf("s%02d", i), baseTime.Add(time.Duration(i)*time.Minute))
	}
	store.delay = 15 * time.Millisecond
	results, err := ListMetadata(context.Background(), store, "sessions", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 40 {
		t.Fatalf("results = %d", len(results))
	}
	if store.maxInFlight > listConcurrency || store.maxInFlight < 2 {
		t.Fatalf("max concurrent reads = %d, want between 2 and %d", store.maxInFlight, listConcurrency)
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].CapturedAt.Before(results[i].CapturedAt) {
			t.Fatalf("results are not newest first at %d", i)
		}
	}
}

// Concurrency must not change which error a listing reports: the first
// failing sidecar in key order, as the sequential read did.
func TestListReportsTheFirstFailingSidecarInKeyOrder(t *testing.T) {
	store := newCountingStore()
	var keys []string
	for i := 0; i < 20; i++ {
		keys = append(keys, putSession(t, store, "codex", fmt.Sprintf("s%02d", i), baseTime))
	}
	store.failGet[keys[3]] = errors.New("synthetic outage")
	store.failGet[keys[12]] = errors.New("synthetic outage")
	for attempt := 0; attempt < 5; attempt++ {
		_, err := ListMetadata(context.Background(), store, "sessions", Filter{})
		if err == nil || !strings.Contains(err.Error(), keys[3]) {
			t.Fatalf("error = %v, want the failure for %s", err, keys[3])
		}
	}
	// A sidecar which does not validate fails the listing too.
	if err := store.Put(context.Background(), keys[0], []byte(`{"schema_version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ListMetadata(context.Background(), store, "sessions", Filter{}); err == nil || !strings.Contains(err.Error(), keys[0]) {
		t.Fatalf("invalid sidecar error = %v", err)
	}
}

func TestFindMetadataKeysReadsKnownHarnessKeysBeforeListing(t *testing.T) {
	store := newCountingStore()
	key := putSession(t, store, "claude", "known", baseTime)
	found, err := FindMetadataKeys(context.Background(), store, "sessions", "known")
	if err != nil {
		t.Fatal(err)
	}
	lists, gets := store.counts()
	if len(found) != 1 || found[0] != key || len(lists) != 0 || len(gets) != len(Harnesses) {
		t.Fatalf("found=%q lists=%q gets=%q", found, lists, gets)
	}

	// A session under a harness this build does not know is still found,
	// through the listing fallback.
	store.reset()
	other := "sessions/other-harness/elsewhere/metadata.json"
	if err := store.Put(context.Background(), other, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	found, err = FindMetadataKeys(context.Background(), store, "sessions", "elsewhere")
	lists, _ = store.counts()
	if err != nil || len(found) != 1 || found[0] != other || len(lists) != 1 {
		t.Fatalf("fallback found=%q lists=%q err=%v", found, lists, err)
	}

	// The same ID under two known harnesses stays ambiguous.
	putSession(t, store, "codex", "known", baseTime)
	found, err = FindMetadataKeys(context.Background(), store, "sessions", "known")
	if err != nil || len(found) != 2 {
		t.Fatalf("ambiguous found=%q err=%v", found, err)
	}

	// A read failure is reported, not mistaken for absence.
	store.failGet["sessions/claude/broken/metadata.json"] = errors.New("synthetic outage")
	if _, err := FindMetadataKeys(context.Background(), store, "sessions", "broken"); err == nil {
		t.Fatal("read failure was treated as a missing session")
	}
	if _, err := FindMetadataKeys(context.Background(), store, "sessions", ".."); err == nil {
		t.Fatal("unsafe session ID accepted")
	}
}

func cacheFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "cache", "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestMetadataCacheServesUnchangedSidecarsAndRefreshesChangedOnes(t *testing.T) {
	store := newCountingStore()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		putSession(t, store, "codex", "s1", baseTime),
		putSession(t, store, "codex", "s2", baseTime.Add(time.Hour)),
		putSession(t, store, "claude", "s3", baseTime.Add(2*time.Hour)),
	}
	options := ListOptions{Cache: cache}
	first, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, gets := store.counts(); len(gets) != 3 {
		t.Fatalf("cold cache gets = %q", gets)
	}

	store.reset()
	second, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, gets := store.counts(); len(gets) != 0 {
		t.Fatalf("warm cache still downloaded %q", gets)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("cached listing differs:\n%s\n%s", a, b)
	}

	// A changed sidecar has a new ETag and is downloaded again.
	store.reset()
	putSession(t, store, "codex", "s1", baseTime.Add(5*time.Hour))
	third, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, gets := store.counts(); len(gets) != 1 || gets[0] != keys[0] {
		t.Fatalf("changed ETag gets = %q", gets)
	}
	if third[0].SessionID != "s1" || !third[0].CapturedAt.Equal(baseTime.Add(5*time.Hour)) {
		t.Fatalf("refreshed sidecar not used: %#v", third[0])
	}

	// --no-cache: every sidecar is downloaded.
	store.reset()
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, gets := store.counts(); len(gets) != 3 {
		t.Fatalf("uncached gets = %q", gets)
	}
}

func TestMetadataCacheEvictsSidecarsNoLongerListed(t *testing.T) {
	store := newCountingStore()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	codexKey := putSession(t, store, "codex", "s1", baseTime)
	putSession(t, store, "claude", "s2", baseTime)
	options := ListOptions{Cache: cache}
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, options); err != nil {
		t.Fatal(err)
	}
	if files := cacheFiles(t, home); len(files) != 2 {
		t.Fatalf("cache files = %q", files)
	}

	// A harness-scoped listing covers only its own prefix: it must not evict
	// the other harness's entry it never listed.
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{Harness: "codex"}, options); err != nil {
		t.Fatal(err)
	}
	if files := cacheFiles(t, home); len(files) != 2 {
		t.Fatalf("harness listing evicted another harness: %q", files)
	}

	// Retention deleted the codex session: the next listing forgets it.
	if err := store.Delete(context.Background(), codexKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{Harness: "codex"}, options); err != nil {
		t.Fatal(err)
	}
	files := cacheFiles(t, home)
	if len(files) != 1 {
		t.Fatalf("deleted sidecar still cached: %q", files)
	}
	var entry metadataCacheEntry
	data, err := os.ReadFile(filepath.Join(home, "cache", "metadata", files[0]))
	if err != nil || json.Unmarshal(data, &entry) != nil || entry.Key != "sessions/claude/s2/metadata.json" {
		t.Fatalf("remaining entry = %s err=%v", data, err)
	}
}

// The cache is private, holds metadata sidecars only, and a damaged entry is
// a miss rather than a failure.
func TestMetadataCacheIsPrivateMetadataOnlyAndDisposable(t *testing.T) {
	store := newCountingStore()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	key := putSession(t, store, "codex", "s1", baseTime)
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(home, "cache"), filepath.Join(home, "cache", "metadata")} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v err=%v", dir, info.Mode().Perm(), err)
		}
	}
	files := cacheFiles(t, home)
	for _, name := range files {
		path := filepath.Join(home, "cache", "metadata", name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v err=%v", name, info.Mode().Perm(), err)
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "source bytes") {
			t.Fatal("source content reached the metadata cache")
		}
	}

	// put refuses anything but a metadata sidecar key.
	cache.put("sessions/codex/s1/source."+fakeSourceSHA+".json.gz", "etag", []byte(`{"not":"metadata"}`))
	if after := cacheFiles(t, home); len(after) != len(files) {
		t.Fatalf("a non-metadata object was cached: %q", after)
	}

	// A damaged entry is downloaded again, not trusted and not fatal.
	if err := os.WriteFile(filepath.Join(home, "cache", "metadata", files[0]), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.reset()
	results, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{Cache: cache})
	if err != nil || len(results) != 1 {
		t.Fatalf("damaged cache broke listing: %v", err)
	}
	if _, gets := store.counts(); len(gets) != 1 || gets[0] != key {
		t.Fatalf("damaged entry was not refreshed: %q", gets)
	}
}
