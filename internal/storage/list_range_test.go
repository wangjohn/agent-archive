package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

// pagingS3 serves ListObjectsV2 over a fixed key set with real paging:
// prefix, start-after, max-keys (capped at pageSize) and continuation tokens.
// With ignoreStartAfter it behaves like a provider that drops the parameter.
type pagingS3 struct {
	keys             []string
	pageSize         int
	ignoreStartAfter bool

	mu       sync.Mutex
	requests []string // start-after|continuation-token of each request
}

func (p *pagingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	p.mu.Lock()
	p.requests = append(p.requests, query.Get("start-after")+"|"+query.Get("continuation-token"))
	p.mu.Unlock()
	start := query.Get("start-after")
	if p.ignoreStartAfter {
		start = ""
	}
	if token := query.Get("continuation-token"); token != "" {
		start = token
	}
	var matched []string
	for _, key := range p.keys {
		if strings.HasPrefix(key, query.Get("prefix")) && key > start {
			matched = append(matched, key)
		}
	}
	type content struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	type result struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}
	truncated, next := false, ""
	if len(matched) > p.pageSize {
		matched = matched[:p.pageSize]
		truncated, next = true, matched[len(matched)-1]
	}
	contents := make([]content, 0, len(matched))
	for _, key := range matched {
		contents = append(contents, content{Key: key, Size: 1})
	}
	out := result{IsTruncated: truncated, NextContinuationToken: next, Contents: contents}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

func (p *pagingS3) requestLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func newPagingStore(t *testing.T, fake *pagingS3) *S3Store {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", Prefix: "agent-archive"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func rangeTestKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("agent-archive/sessions/claude/k%02d", i)
	}
	sort.Strings(keys)
	return keys
}

func objectKeys(objects []Object) []string {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = object.Key
	}
	return keys
}

func TestS3StoreListRangeStartsAfterAndStopsPastThrough(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(10), pageSize: 3}
	store := newPagingStore(t, fake)
	got, err := store.ListRange(context.Background(), "sessions", "sessions/claude/k02", "sessions/claude/k06")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sessions/claude/k03", "sessions/claude/k04", "sessions/claude/k05", "sessions/claude/k06"}
	if !slices.Equal(objectKeys(got), want) {
		t.Fatalf("ListRange = %v, want %v", objectKeys(got), want)
	}
	// The first request starts after the full bucket key; the second page
	// (k06, k07, k08) passes through, so no third request is made.
	requests := fake.requestLog()
	if len(requests) != 2 || requests[0] != "agent-archive/sessions/claude/k02|" {
		t.Fatalf("requests = %v, want start-after on the first and paging to stop after the second", requests)
	}
}

func TestS3StoreListRangeUnboundedIsTheWholeListing(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(7), pageSize: 3}
	store := newPagingStore(t, fake)
	got, err := store.ListRange(context.Background(), "sessions", "", "")
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.List(context.Background(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(objectKeys(got), objectKeys(all)) || len(got) != 7 {
		t.Fatalf("ListRange(\"\", \"\") = %v, List = %v", objectKeys(got), objectKeys(all))
	}
	if requests := fake.requestLog(); requests[0] != "|" {
		t.Fatalf("an unbounded range sent start-after: %v", requests)
	}
}

// A provider that ignored start-after would start every range from the
// beginning. The store enforces the range itself, so adjacent ranges still
// share no key and miss none.
func TestS3StoreListRangeEnforcesBoundsWhenStartAfterIsIgnored(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(12), pageSize: 4, ignoreStartAfter: true}
	store := newPagingStore(t, fake)
	var joined []string
	bounds := []string{"", "sessions/claude/k03", "sessions/claude/k08", ""}
	for i := 0; i+1 < len(bounds); i++ {
		got, err := store.ListRange(context.Background(), "sessions", bounds[i], bounds[i+1])
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, objectKeys(got)...)
	}
	all, err := store.List(context.Background(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(joined, objectKeys(all)) {
		t.Fatalf("joined ranges = %v, want %v", joined, objectKeys(all))
	}
}

func TestS3StoreListRangeRejectsAnUnsafeStart(t *testing.T) {
	store := newPagingStore(t, &pagingS3{pageSize: 3})
	if _, err := store.ListRange(context.Background(), "sessions", "../escape", ""); err == nil {
		t.Fatal("ListRange accepted a start key outside the store prefix")
	}
}

func TestS3StoreListRangePropagatesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRange(context.Background(), "sessions", "", ""); err == nil || !strings.Contains(err.Error(), strconv.Itoa(http.StatusForbidden)) {
		t.Fatalf("ListRange error = %v, want the 403", err)
	}
}
