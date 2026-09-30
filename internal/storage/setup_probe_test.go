package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

func newProbeS3Store(t *testing.T, handler http.HandlerFunc, accessKey string) *S3Store {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider(accessKey, "secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive", Prefix: "agent-archive"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// The probe asks S3 for at most one object under the setup test folder and
// sends nothing else, even when the bucket has many more objects there.
func TestProbeMakesOneBoundedListRequest(t *testing.T) {
	var requests []string
	store := newProbeS3Store(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Query().Get("prefix")+"|"+r.URL.Query().Get("max-keys"))
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>more</NextContinuationToken><Contents><Key>agent-archive/.setup-test/old.json</Key><Size>1</Size></Contents></ListBucketResult>`)
	}, "test")
	if err := Probe(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET agent-archive/.setup-test/|1"}; !slices.Equal(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

// listOnlyStore is an ObjectStore that cannot page and refuses writes.
type listOnlyStore struct {
	ObjectStore
	prefixes []string
}

func (s *listOnlyStore) List(_ context.Context, prefix string) ([]Object, error) {
	s.prefixes = append(s.prefixes, prefix)
	return nil, nil
}

func (s *listOnlyStore) Put(context.Context, string, []byte) error {
	panic("Probe must not write")
}

// A store without paging is listed under the setup test folder only, never
// the whole archive.
func TestProbeListsOnlyTheSetupFolderWithoutPaging(t *testing.T) {
	store := &listOnlyStore{}
	if err := Probe(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.prefixes, []string{".setup-test/"}) {
		t.Fatalf("listed %v", store.prefixes)
	}
}

// A refusal comes back wrapped, so Diagnose still explains it, and names the
// folder the probe listed.
func TestProbeFailureIsDiagnosable(t *testing.T) {
	store := newProbeS3Store(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>InvalidAccessKeyId</Code><Message>The access key does not exist.</Message></Error>`)
	}, "wrong")
	err := Probe(context.Background(), store)
	if err == nil {
		t.Fatal("Probe accepted a refused key")
	}
	if d := Diagnose(err); d.Cause != CauseNoCredentials {
		t.Fatalf("cause = %s (%v)", d.Cause, err)
	}
	if !strings.Contains(err.Error(), `"agent-archive/.setup-test/"`) {
		t.Fatalf("error does not name the folder: %v", err)
	}
}
