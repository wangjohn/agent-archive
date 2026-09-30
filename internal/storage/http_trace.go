package storage

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// tracedClient records each request as a trace span, named by operation
// only, from sending it until its response body is closed, so a listing
// page's download time counts. With tracing off it only checks a pointer.
type tracedClient struct {
	inner aws.HTTPClient
}

func (c tracedClient) Do(request *http.Request) (*http.Response, error) {
	if !trace.Enabled() {
		return c.inner.Do(request)
	}
	span := trace.Start("request " + operation(request))
	response, err := c.inner.Do(request)
	if err != nil || response == nil || response.Body == nil {
		span.End()
		return response, err
	}
	response.Body = &tracedBody{ReadCloser: response.Body, span: span}
	return response, nil
}

// operation names what a request does, from its method and whether it is a
// listing. Nothing from the key or bucket is used.
func operation(request *http.Request) string {
	if request.Method == http.MethodGet && request.URL.Query().Has("list-type") {
		return "list"
	}
	return strings.ToLower(request.Method)
}

type tracedBody struct {
	io.ReadCloser
	span  *trace.Span
	once  sync.Once
	bytes int
}

func (b *tracedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += n
	return n, err
}

func (b *tracedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		b.span.Count("bytes", b.bytes)
		b.span.End()
	})
	return err
}
