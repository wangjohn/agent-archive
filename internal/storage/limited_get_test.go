package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

type streamingBody struct{ read int }

func (b *streamingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}
func (*streamingBody) Close() error { return nil }

type boundedHTTP struct{ body *streamingBody }

func (h boundedHTTP) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: h.body, ContentLength: 1 << 30}, nil
}

func TestS3LimitedGetStopsBeforeLargeBodyAllocation(t *testing.T) {
	t.Parallel()
	body := &streamingBody{}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: boundedHTTP{body}}
	store, e := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1 << 20})
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.GetLimited(context.Background(), "machines/id.json", 16<<10)
	if !errors.Is(e, ErrObjectTooLarge) || body.read > (16<<10)+1 {
		t.Fatalf("err=%v bytes=%d", e, body.read)
	}
}

func TestLimitedGetDoesNotChangeArchiveGetLimit(t *testing.T) {
	t.Parallel()
	server := newFakeS3Server()
	defer server.Close()
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}
	s, e := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "test", MaxGetBytes: 64})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Put(context.Background(), "body", []byte(strings.Repeat("a", 32))); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetLimited(context.Background(), "body", 8); !errors.Is(e, ErrObjectTooLarge) {
		t.Fatal(e)
	}
	b, e := s.Get(context.Background(), "body")
	if e != nil || len(b) != 32 {
		t.Fatalf("%d %v", len(b), e)
	}
	if _, e = s.GetLimited(context.Background(), "body", 0); e == nil {
		t.Fatal("zero limit accepted")
	}
}

var _ io.ReadCloser = (*streamingBody)(nil)
