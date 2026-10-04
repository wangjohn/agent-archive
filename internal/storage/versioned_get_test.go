package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestVersionedGetPreservesResponseOpaqueValidator(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"opaque/version:KMS-not-a-digest"`)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("metadata"))
		}
	}))
	defer server.Close()
	cfg := aws.Config{Region: "test", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "test", MaxGetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	data, etag, err := store.GetVersioned(context.Background(), "metadata")
	if err != nil || string(data) != "metadata" || etag != "opaque/version:KMS-not-a-digest" {
		t.Fatalf("body=%q validator=%q err=%v", data, etag, err)
	}
	info, err := store.Stat(context.Background(), "metadata")
	if err != nil || info.ETag != etag {
		t.Fatalf("HEAD validator=%q err=%v", info.ETag, err)
	}
}
