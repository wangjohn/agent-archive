package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionCatalogRefreshPagesAndWarmBodies(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 31 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
	}
	reads := 0
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{BodyRead: func(string, bool) { reads++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if reads != 31 {
		t.Fatalf("cold bodies=%d", reads)
	}
	reads = 0
	store.reset()
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("warm decoded %d bodies", reads)
	}
	q := CatalogQuery{Metadata: MetadataQuery{Limit: 7, Order: ActivityOrder}}
	first, err := c.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 31 || len(first.Rows) != 7 || !first.Complete {
		t.Fatalf("page=%+v", first)
	}
	all := append([]CatalogRow(nil), first.Rows...)
	q.Cursor = first.Next
	for q.Cursor != "" {
		p, e := c.Query(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, p.Rows...)
		q.Cursor = p.Next
	}
	oracle, err := ListMetadataWithOptions(ctx, store, "sessions/", Filter{}, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(oracle) {
		t.Fatal("lost rows")
	}
	for i, row := range all {
		if row.Summary.SessionID != oracle[i].SessionID {
			t.Fatalf("order[%d]=%s != %s", i, row.Summary.SessionID, oracle[i].SessionID)
		}
		if len(row.Hash) != 64 {
			t.Fatal("missing body hash")
		}
	}
	q.Cursor = first.Next
	q.Words = []string{"changed project label binding"}
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("query cursor=%v", err)
	}
	if err = store.Delete(ctx, all[0].Key); err != nil {
		t.Fatal(err)
	}
	h, err = DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	q.Words = nil
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("generation cursor=%v", err)
	}
	q.Cursor = ""
	p, err := c.Query(ctx, q)
	if err != nil || p.Total != 30 {
		t.Fatalf("deleted total=%d err=%v", p.Total, err)
	}
	q.Cursor = p.Next
	putSession(t, store, "codex", all[1].Summary.SessionID, baseTime.Add(2*time.Hour))
	h, err = DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Query(ctx, q); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("changed cursor=%v", err)
	}
	q.Cursor = ""
	p, err = c.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if p.Total != 30 || p.Rows[0].Summary.SessionID != all[1].Summary.SessionID || p.Rows[0].ETag == all[1].ETag || p.Rows[0].Hash == all[1].Hash {
		t.Fatal("changed revision did not reconcile")
	}
}

func TestSessionCatalogCancellationConcurrentAndPrivacy(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	// Reopening an adopted index also repairs permissive database/WAL modes.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := filepath.Join(home, "cache", "catalog", "sessions.sqlite"+suffix)
		if _, e := os.Stat(path); e == nil {
			if e = os.Chmod(path, 0644); e != nil {
				t.Fatal(e)
			}
		}
	}
	other, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = c.Refresh(canceled, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	page, err := c.Query(ctx, CatalogQuery{})
	if err != nil || page.Total != 0 {
		t.Fatal("canceled rebuild committed")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, db := range []*SQLiteSessionCatalog{c, other} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- db.Refresh(ctx, h) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if err = filepath.WalkDir(filepath.Join(home, "cache", "catalog"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := d.Info()
		if e != nil {
			return e
		}
		want := os.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if i.Mode().Perm() != want {
			return fmt.Errorf("%s mode=%o", path, i.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCatalogCorruptionRebuild(t *testing.T) {
	home := t.TempDir()
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "cache", "catalog")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "sessions.sqlite"), []byte("damaged index"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(context.Background(), cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	p, err := c.Query(context.Background(), CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("rebuilt=%+v err=%v", p, err)
	}
}

func TestSessionCatalogTypedFiltersEqualExhaustiveOracle(t *testing.T) {
	ctx := context.Background()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	summaries := []SearchSummary{
		{SessionID: "one", Harness: archive.Harness{Name: "codex"}, CapturedAt: baseTime, ParentSessionID: "parent", Replay: &archive.Replay{}, Models: []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "gpt-test"}}}, SkillsUsed: []archive.SkillUse{{Name: "used", SHA256: "abc"}}},
		{SessionID: "two", Harness: archive.Harness{Name: "claude"}, CapturedAt: baseTime.Add(time.Nanosecond), Parser: archive.ParserInfo{Status: archive.ParserStatusComplete}, SkillsAvailable: []archive.SkillSnapshot{{Name: "available", SHA256: "def"}}},
		{SessionID: "three", Harness: archive.Harness{Name: "codex"}, CapturedAt: baseTime.Add(time.Hour), Parser: archive.ParserInfo{Status: archive.ParserStatusComplete}, CaptureGaps: []archive.CaptureGap{{Code: "gap"}}},
	}
	for _, s := range summaries {
		data, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.db.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?,?)", s.SessionID, "etag", "hash", catalogTime(s.CapturedAt), catalogTime(s.CapturedAt), data, catalogSearch(s), strings.ToLower(s.SessionID), s.ProjectName == ""); e != nil {
			t.Fatal(e)
		}
	}
	filters := []Filter{{}, {Harness: "codex"}, {Harness: "missing"}, {From: baseTime.Add(time.Nanosecond), To: baseTime.Add(time.Nanosecond)}, {From: baseTime.Add(24 * time.Hour)}, {Replays: ReplaysOnly}, {Replays: ReplaysHidden}, {Model: "gpt-test"}, {Model: "unknown"}, {Skill: "used"}, {Skill: "available", SkillUsage: SkillUsageAvailable}, {SkillSHA256: "missing"}, {RequireCompleteCoverage: true}}
	for _, f := range filters {
		for _, top := range []bool{false, true} {
			var want []string
			for i := len(summaries) - 1; i >= 0; i-- {
				m := summaries[i].Metadata()
				if matches(m, f) && (!top || m.ParentSessionID == "") {
					want = append(want, m.SessionID)
				}
			}
			q := CatalogQuery{Metadata: MetadataQuery{Filter: f, TopLevelOnly: top, Limit: 1}}
			var got []string
			for {
				page, e := c.Query(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				if page.Total != len(want) || !page.Complete {
					t.Fatalf("filter=%+v total=%d want=%d", f, page.Total, len(want))
				}
				for _, r := range page.Rows {
					got = append(got, r.Summary.SessionID)
				}
				if page.Next == "" {
					break
				}
				q.Cursor = page.Next
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filter=%+v top=%t got=%v want=%v", f, top, got, want)
			}
		}
	}
}

type interruptedCatalogStore struct {
	*countingStore
	started chan struct{}
}

func (s *interruptedCatalogStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, "", ctx.Err()
}

func TestSessionCatalogCancelColdReadsJoinsWorkers(t *testing.T) {
	store := &interruptedCatalogStore{countingStore: newCountingStore(), started: make(chan struct{}, 1)}
	for i := range 20 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime)
	}
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(context.Background(), cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(context.Background(), store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Refresh(ctx, h) }()
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cold reads did not start")
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel=%v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not join")
	}
	p, err := c.Query(context.Background(), CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("partial rebuild visible=%+v err=%v", p, err)
	}
}

func TestSessionCatalogRejectsRevisionRaceAndBadHash(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	key := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	body, etag, err := store.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := listingindex.NewRevision(key, body, etag)
	if err != nil {
		t.Fatal(err)
	}
	revision.Hash = strings.Repeat("0", 64)
	h.Revisions = map[RevisionID]listingindex.Revision{{Key: key, ETag: etag}: revision}
	if err = c.Refresh(ctx, h); !errors.Is(err, ErrRefreshRequired) {
		t.Fatalf("bad hash=%v", err)
	}
	h.Revisions = nil
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime.Add(time.Hour))
	if err = c.Refresh(ctx, h); !errors.Is(err, ErrRefreshRequired) {
		t.Fatalf("race=%v", err)
	}
	p, err := c.Query(ctx, CatalogQuery{})
	if err != nil || p.Total != 0 {
		t.Fatalf("race committed=%+v err=%v", p, err)
	}
}

func TestSessionCatalogWordsAreUnicodeAndLabelSafeCandidates(t *testing.T) {
	ctx := context.Background()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, newCountingStore(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	summaries := []SearchSummary{{SessionID: "abcd123456", Name: "ÉCOLE", ProjectName: "published"}, {SessionID: "212abcdef", Title: "different", ProjectName: "published", PullRequests: []archive.PullRequestLink{{Number: 21}}}, {SessionID: "unlabeled", Name: "different"}}
	for _, s := range summaries {
		data, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = c.db.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?,?,?,?)", s.SessionID, "etag", "hash", catalogTime(baseTime), catalogTime(baseTime), data, catalogSearch(s), strings.ToLower(s.SessionID), s.ProjectName == ""); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		word string
		want []string
	}{{"école", []string{"abcd123456", "unlabeled"}}, {"abcd1234", []string{"abcd123456", "unlabeled"}}, {"#21", []string{"212abcdef", "unlabeled"}}, {"212", []string{"212abcdef", "unlabeled"}}, {"configured label", []string{"unlabeled"}}} {
		p, e := c.Query(ctx, CatalogQuery{Words: []string{tc.word}})
		if e != nil {
			t.Fatal(e)
		}
		var ids []string
		for _, r := range p.Rows {
			ids = append(ids, r.Summary.SessionID)
		}
		if !reflect.DeepEqual(ids, tc.want) {
			t.Fatalf("word=%q got=%v want=%v", tc.word, ids, tc.want)
		}
	}
}

func TestSessionCatalogRejectsPartialCanonicalSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if !h.CanonicalComplete {
		t.Fatal("discovery omitted proof")
	}
	if err = c.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	for _, partial := range []HeaderSnapshot{{}, {Canonical: h.Canonical}} {
		if err = c.Refresh(ctx, partial); err == nil {
			t.Fatal("accepted unproven canonical snapshot")
		}
	}
	p, err := c.Query(ctx, CatalogQuery{})
	if err != nil || p.Total != 1 || !p.Complete {
		t.Fatalf("partial snapshot altered catalog=%+v err=%v", p, err)
	}
}

func TestSessionCatalogProjectionStoresOnlySearchFields(t *testing.T) {
	m := archive.Metadata{Name: "published native name", Title: "filtered prompt preview", SourceBundle: archive.SourceReference{Key: "private-source-pointer"}, Replay: &archive.Replay{RunID: "private-replay-run"}, CaptureGaps: []archive.CaptureGap{{Code: "gap", Detail: "private-gap-detail"}}, PullRequests: []archive.PullRequestLink{{Number: 21, URL: "private-pr-url", Repository: "private-repository"}}, GitActivity: []archive.GitEvent{{Kind: archive.GitEventCommit, SHA: "private-commit"}, {Kind: archive.GitEventPRCreated, PRNumber: 22, URL: "private-git-url"}}}
	summary := summarize(m)
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-source-pointer", "private-replay-run", "private-gap-detail", "private-pr-url", "private-repository", "private-commit", "private-git-url", "source_bundle", "counts", "history"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("projection retained %q", secret)
		}
	}
	if !strings.Contains(string(data), m.Name) || !strings.Contains(string(data), m.Title) || summary.PullRequests[0].Number != 21 || summary.GitActivity[0].PRNumber != 22 {
		t.Fatal("lost published search fields")
	}
}

// A command must not borrow another handle's authority after its own discovery.
func TestSessionCatalogFirstQueryBindsRefreshedGeneration(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore()
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	first, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	other, err := OpenSessionCatalog(ctx, cache, store, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	h, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	unready, err := other.Query(ctx, CatalogQuery{})
	if err != nil || unready.Complete || unready.Total != 0 {
		t.Fatalf("unrefreshed view=%+v err=%v", unready, err)
	}
	if err = other.Refresh(ctx, HeaderSnapshot{CanonicalComplete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = first.Query(ctx, CatalogQuery{}); !errors.Is(err, ErrStaleCatalogCursor) {
		t.Fatalf("first query accepted replaced generation: %v", err)
	}
	if err = first.Refresh(ctx, h); err != nil {
		t.Fatal(err)
	}
	page, err := first.Query(ctx, CatalogQuery{})
	if err != nil || !page.Complete || page.Total != 1 {
		t.Fatalf("refreshed view=%+v err=%v", page, err)
	}
}
