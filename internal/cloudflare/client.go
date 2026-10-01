// Package cloudflare is the small slice of Cloudflare's management API that
// guided R2 setup needs: find the account, create a bucket, mint a
// bucket-scoped API token, and check the bucket's public-access settings.
//
// It is used only at setup time, with a bootstrap token the person pastes
// once. That token lives in a Client, is never written anywhere, and is
// dropped with Discard. The runtime credentials the archive stores are
// different values that are used against the S3 endpoint, never against this
// API.
//
// Nothing here has been run against the real Cloudflare API yet: the request
// and response shapes come from Cloudflare's published API reference, and the
// items that documentation leaves open are listed under "Live acceptance" in
// dev/contributing/testing.md.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Cloudflare's v4 API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// maxResponseBytes bounds what a response may hold. Every answer this
// package reads is a few kilobytes.
const maxResponseBytes = 1 << 20

// Options adjust a Client. The zero value talks to Cloudflare.
type Options struct {
	// BaseURL replaces DefaultBaseURL. Tests point it at an httptest
	// server; nothing in production sets it, so a token is never sent to
	// another host.
	BaseURL string
	// HTTPClient sends the requests. Nil means a client with a 30 second
	// timeout that follows no redirects, so the token stays with the one
	// host it was meant for.
	HTTPClient *http.Client
	// Sleep waits between a rate-limited request and its retry. Nil means
	// a timer that also ends when ctx does.
	Sleep func(ctx context.Context, d time.Duration) error
	// MaxRetries is how many times a rate-limited (429) request is sent
	// again. Zero means 3.
	MaxRetries int
	// MaxWait is the longest single wait honored from a Retry-After
	// header; a longer one ends the request with the rate-limit error.
	// Zero means 30 seconds.
	MaxWait time.Duration
}

// Client talks to Cloudflare with one bearer token.
type Client struct {
	base       string
	token      []byte
	http       *http.Client
	sleep      func(ctx context.Context, d time.Duration) error
	maxRetries int
	maxWait    time.Duration
}

var _ API = (*Client)(nil)

// New returns a Client that authenticates with token.
func New(token string, opts Options) *Client {
	c := &Client{
		base:       strings.TrimRight(opts.BaseURL, "/"),
		token:      []byte(token),
		http:       opts.HTTPClient,
		sleep:      opts.Sleep,
		maxRetries: opts.MaxRetries,
		maxWait:    opts.MaxWait,
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.maxRetries == 0 {
		c.maxRetries = 3
	}
	if c.maxWait == 0 {
		c.maxWait = 30 * time.Second
	}
	return c
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Discard forgets the token: the bytes held here are zeroed and every later
// request fails. Go strings are immutable, so a copy made while building a
// request header can outlive this; the guarantee is that the Client itself
// keeps nothing.
func (c *Client) Discard() {
	for i := range c.token {
		c.token[i] = 0
	}
	c.token = nil
}

// Error is a failed call: a Cloudflare answer other than success, or no
// answer at all.
type Error struct {
	// Op is what was being done, such as "create bucket".
	Op string
	// Status is the HTTP status, or 0 when there was no response.
	Status int
	// Codes are Cloudflare's numeric error codes, when it sent any.
	Codes []int
	// Messages are Cloudflare's error messages, with the token removed.
	Messages []string
	// RetryAfter is what a 429 asked us to wait, when it said.
	RetryAfter time.Duration
	// Err is the transport failure when there was no response.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("cloudflare: " + e.Op)
	if e.Status != 0 {
		b.WriteString(": HTTP " + strconv.Itoa(e.Status))
	}
	if len(e.Messages) > 0 {
		b.WriteString(": " + strings.Join(e.Messages, "; "))
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the transport failure, if any.
func (e *Error) Unwrap() error { return e.Err }

// Unauthorized reports that Cloudflare did not accept the token at all.
func (e *Error) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// Forbidden reports that the token is valid but lacks a permission.
func (e *Error) Forbidden() bool { return e.Status == http.StatusForbidden }

// NotFound reports that the account or bucket does not exist for this token.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// RateLimited reports that Cloudflare's rate limit stopped the call.
func (e *Error) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// ServerError reports a failure on Cloudflare's side.
func (e *Error) ServerError() bool { return e.Status >= 500 }

// AlreadyExists reports that Cloudflare said the name is taken: a conflict,
// or a message saying it already exists (how Cloudflare words a bucket name
// collision).
func (e *Error) AlreadyExists() bool {
	if e.Status == http.StatusConflict {
		return true
	}
	for _, m := range e.Messages {
		if strings.Contains(strings.ToLower(m), "already exists") {
			return true
		}
	}
	return false
}

// envelope is the JSON wrapper around every Cloudflare answer.
type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		Count      int `json:"count"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

// call is one request.
type call struct {
	op     string
	method string
	path   string
	query  url.Values
	header map[string]string
	body   any
}

// do sends cl and decodes the answer's result into out, when out is not nil.
// A 429 is sent again, after the wait Cloudflare asked for, up to MaxRetries
// times and never for longer than MaxWait at once: a rate-limited request
// did nothing, so repeating even a POST is safe.
func (c *Client) do(ctx context.Context, cl call, out any) (envelope, error) {
	var payload []byte
	if cl.body != nil {
		var err error
		if payload, err = json.Marshal(cl.body); err != nil {
			return envelope{}, &Error{Op: cl.op, Err: err}
		}
	}
	for attempt := 0; ; attempt++ {
		env, err := c.once(ctx, cl, payload, out)
		var apiErr *Error
		if !errors.As(err, &apiErr) || !apiErr.RateLimited() || attempt >= c.maxRetries {
			return env, err
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = 2 * time.Second << attempt
		}
		if wait > c.maxWait {
			return env, err
		}
		if e := c.sleep(ctx, wait); e != nil {
			return env, &Error{Op: cl.op, Err: e}
		}
	}
}

func (c *Client) once(ctx context.Context, cl call, payload []byte, out any) (envelope, error) {
	if len(c.token) == 0 {
		return envelope{}, &Error{Op: cl.op, Err: errors.New("the API token was already discarded")}
	}
	target := c.base + cl.path
	if len(cl.query) > 0 {
		target += "?" + cl.query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, target, body)
	if err != nil {
		return envelope{}, &Error{Op: cl.op, Err: errors.New("invalid request")}
	}
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agent-archive")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cl.header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return envelope{}, &Error{Op: cl.op, Err: c.scrubbed(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return envelope{}, &Error{Op: cl.op, Status: resp.StatusCode, Err: c.scrubbed(err)}
	}
	var env envelope
	decodeErr := json.Unmarshal(raw, &env)
	if resp.StatusCode/100 != 2 || decodeErr == nil && !env.Success {
		apiErr := &Error{Op: cl.op, Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
		for _, e := range env.Errors {
			apiErr.Codes = append(apiErr.Codes, e.Code)
			if e.Message != "" {
				apiErr.Messages = append(apiErr.Messages, c.scrub(e.Message))
			}
		}
		if apiErr.Status/100 == 2 {
			// A 2xx that says it did not succeed.
			apiErr.Status = http.StatusBadGateway
		}
		return env, apiErr
	}
	if decodeErr != nil {
		return env, &Error{Op: cl.op, Status: resp.StatusCode, Err: errors.New("the answer could not be read as JSON")}
	}
	if out != nil {
		// An answer that says success but carries no result is not an empty
		// result: nothing can be concluded from it, so it is unreadable.
		if len(env.Result) == 0 || bytes.Equal(bytes.TrimSpace(env.Result), []byte("null")) {
			return env, &Error{Op: cl.op, Status: resp.StatusCode, Err: errors.New("the answer had no result")}
		}
		if err := json.Unmarshal(env.Result, out); err != nil {
			return env, &Error{Op: cl.op, Status: resp.StatusCode, Err: errors.New("the answer did not have the expected fields")}
		}
	}
	return env, nil
}

// retryAfter reads a Retry-After header of whole seconds. A date, or
// anything else, reads as "not said".
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// scrub removes the token from text that came from anywhere else, so an
// error can never carry it.
func (c *Client) scrub(text string) string {
	if len(c.token) == 0 {
		return text
	}
	return strings.ReplaceAll(text, string(c.token), "[redacted]")
}

func (c *Client) scrubbed(err error) error {
	return errors.New(c.scrub(err.Error()))
}

func accountPath(account string) string {
	return "/accounts/" + url.PathEscape(account)
}

func bucketPath(account, bucket string) string {
	return accountPath(account) + "/r2/buckets/" + url.PathEscape(bucket)
}

// jurisdictionHeader is the header that names an R2 jurisdiction. Buckets
// in a jurisdiction are reached only with it.
func jurisdictionHeader(jurisdiction string) map[string]string {
	if jurisdiction == "" {
		return nil
	}
	return map[string]string{"cf-r2-jurisdiction": jurisdiction}
}
