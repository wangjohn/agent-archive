package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// Each request is a span named by its operation alone, lasting until its
// body is closed, with the bytes it downloaded. Not parallel: the recorder
// is process-wide.
func TestRequestsAreTracedByOperationOnly(t *testing.T) {
	server := newFakeS3Server()
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", Prefix: "agent-archive"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "sessions/secret-id/metadata.json", []byte(`{"title":"private words"}`)); err != nil {
		t.Fatal(err)
	}
	disable := trace.Enable()
	defer disable()
	if _, err := store.List(ctx, "sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "sessions/secret-id/metadata.json"); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	trace.Write(&b)
	got := b.String()
	if !strings.Contains(got, "request list") || !strings.Contains(got, "request get") || !strings.Contains(got, "bytes ") {
		t.Fatalf("trace lacks the requests:\n%s", got)
	}
	if strings.Contains(got, "put") {
		t.Fatalf("a request made before tracing began was recorded:\n%s", got)
	}
	_, body, _ := strings.Cut(got, "\n")
	for _, leak := range []string{"secret-id", "private", "archive", "metadata.json", "sessions"} {
		if strings.Contains(body, leak) {
			t.Fatalf("trace contains %q:\n%s", leak, got)
		}
	}
}
