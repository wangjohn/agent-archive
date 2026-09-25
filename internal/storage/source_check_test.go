package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

func TestS3StatReportsTheStoredChecksum(t *testing.T) {
	data := []byte("object bytes")
	digest := sha256.Sum256(data)
	header := base64.StdEncoding.EncodeToString(digest[:])
	for _, test := range []struct {
		name       string
		checksum   string
		checksumTy string
		status     int
		wantSHA    string
		wantErr    error
	}{
		{"full object", header, "FULL_OBJECT", http.StatusOK, SHA256Hex(data), nil},
		{"no checksum", "", "", http.StatusOK, "", nil},
		{"composite", header + "-3", "COMPOSITE", http.StatusOK, "", nil},
		{"composite with a whole digest", header, "COMPOSITE", http.StatusOK, "", nil},
		{"missing", "", "", http.StatusNotFound, "", ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
					return
				}
				if r.Header.Get("x-amz-checksum-mode") != "ENABLED" {
					http.Error(w, "checksum mode not requested", http.StatusBadRequest)
					return
				}
				if test.checksum != "" {
					w.Header().Set("x-amz-checksum-sha256", test.checksum)
					w.Header().Set("x-amz-checksum-type", test.checksumTy)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
			store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive"})
			if err != nil {
				t.Fatal(err)
			}
			info, err := store.Stat(context.Background(), "sessions/codex/id/source.x.jsonl.gz")
			if !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, test.wantErr)
			}
			if err == nil && (info.SHA256 != test.wantSHA || info.Size != int64(len(data))) {
				t.Fatalf("info = %#v, want sha %q size %d", info, test.wantSHA, len(data))
			}
		})
	}
}
