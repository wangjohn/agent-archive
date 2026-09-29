package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

func TestPrefixRejectsEscapes(t *testing.T) {
	for _, test := range []struct {
		prefix string
		key    string
	}{
		{"/private", "x"}, {"private/../other", "x"}, {"private", "/x"}, {"private", "../x"}, {"private", `dir\\x`},
	} {
		if _, err := Prefix(test.prefix, test.key); err == nil {
			t.Errorf("Prefix(%q, %q) accepted an unsafe path", test.prefix, test.key)
		}
	}
}

func TestS3StoreFakeHTTPRoundTrip(t *testing.T) {
	server := newFakeS3Server()
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	client := NewClient(awsCfg, server.URL, true, 1)
	store, err := NewS3Store(S3StoreOptions{Client: client, Bucket: "archive", Prefix: "agent-archive", MaxGetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello storage")
	if err := store.Put(context.Background(), "sessions/id/source", payload); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "sessions/id/source")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	items, err := store.List(context.Background(), "sessions/id")
	if err != nil || len(items) != 1 || items[0].Key != "sessions/id/source" {
		t.Fatalf("List = %#v, %v", items, err)
	}
	// S3 quotes the ETag; the store reports the bare MD5 of a single-part
	// object, which the reader's metadata cache checks bytes against.
	if items[0].ETag != md5Hex(payload) {
		t.Fatalf("List ETag = %q, want bare %q", items[0].ETag, md5Hex(payload))
	}
	if err := store.Delete(context.Background(), "sessions/id/source"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "sessions/id/source"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get error = %v", err)
	}
}

func TestS3StoreListPageUsesContinuation(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Query().Get("continuation-token")+"|"+r.URL.Query().Get("max-keys"))
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>agent-archive/listing/v1/a</Key><Size>1</Size></Contents></ListBucketResult>`)
		} else {
			_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>agent-archive/listing/v1/b</Key><Size>1</Size></Contents></ListBucketResult>`)
		}
	}))
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", Prefix: "agent-archive"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ListPage(context.Background(), "listing/v1/", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ListPage(context.Background(), "listing/v1/", first.Next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Next != "next" || second.Next != "" || len(first.Objects) != 1 || first.Objects[0].Key != "listing/v1/a" || len(second.Objects) != 1 || second.Objects[0].Key != "listing/v1/b" || len(calls) != 2 || calls[0] != "|1" || calls[1] != "next|1" {
		t.Fatalf("pages first=%+v second=%+v calls=%v", first, second, calls)
	}
}

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3Server() *httptest.Server {
	fake := &fakeS3{objects: make(map[string][]byte)}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/archive/")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		//lint:ignore LV1001 r.Method is an arbitrary request method; the cases are net/http's own constants
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			fake.objects[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Has("list-type") {
				writeListResponse(w, fake.objects, r.URL.Query().Get("prefix"))
				return
			}
			body, ok := fake.objects[key]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodDelete:
			delete(fake.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeListResponse(w http.ResponseWriter, objects map[string][]byte, prefix string) {
	type content struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
		ETag string `xml:"ETag"`
	}
	type result struct {
		XMLName  xml.Name  `xml:"ListBucketResult"`
		Contents []content `xml:"Contents"`
	}
	var out result
	for key, value := range objects {
		if strings.HasPrefix(key, prefix) {
			out.Contents = append(out.Contents, content{Key: key, Size: len(value), ETag: `"` + md5Hex(value) + `"`})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

// md5Hex is the ETag an S3-compatible store reports for a single-part,
// non-KMS object. It is an identity for ETag comparison, not a security hash.
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
