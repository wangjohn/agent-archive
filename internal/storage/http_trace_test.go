package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// slowBody's first Read waits, as a listing page's download does.
type slowBody struct {
	delay time.Duration
	done  bool
}

func (b *slowBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	time.Sleep(b.delay)
	b.done = true
	return copy(p, "abc"), nil
}

type slowBodyClient struct{ delay time.Duration }

func (c slowBodyClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&slowBody{delay: c.delay})}, nil
}

// A request's span covers reading its body and ends when the body is
// closed: not when the headers arrive, and not whenever the trace is
// written.
func TestRequestSpanLastsUntilTheBodyIsClosed(t *testing.T) {
	const bodyTime = 40 * time.Millisecond
	disable := trace.Enable()
	defer disable()
	client := tracedClient{inner: slowBodyClient{delay: bodyTime}}
	response, err := client.Do(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://store/key", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * bodyTime) // an unended span would run on through this
	var b strings.Builder
	trace.Write(&b)
	var line string
	for l := range strings.SplitSeq(b.String(), "\n") {
		if strings.Contains(l, "request get") {
			line = l
		}
	}
	_, after, _ := strings.Cut(line, "request get")
	fields := strings.Fields(after)
	if len(fields) == 0 {
		t.Fatalf("no request span:\n%s", b.String())
	}
	took, err := time.ParseDuration(fields[0])
	if err != nil {
		t.Fatalf("span timing %q: %v", fields[0], err)
	}
	if took < bodyTime || took >= 5*bodyTime || strings.Contains(line, "unfinished") || !strings.Contains(line, "bytes 3") {
		t.Fatalf("request span %q, want at least the %s body read, ended at Close, with its 3 bytes", line, bodyTime)
	}
}
