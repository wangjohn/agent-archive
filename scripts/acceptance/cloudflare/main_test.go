package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReportRefusesSecretsAndUsesPrivateMode(t *testing.T) {
	r := &runner{path: filepath.Join(t.TempDir(), "report.json"), secrets: []string{"canary-secret-should-not-be-saved"}, r: report{RunID: "synthetic"}}
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report must be private", err)
	}
	r.r.Checks = []check{{Detail: "canary-secret-should-not-be-saved"}}
	if err := r.save(); err == nil {
		t.Fatal("secret-bearing report accepted")
	}
	data, err := os.ReadFile(r.path)
	if err != nil || strings.Contains(string(data), r.secrets[0]) {
		t.Fatal("secret escaped into report", err)
	}
}

func TestCleanupDeletesOnlyConfirmedNewBucketsAndChecksEnvelope(t *testing.T) {
	for _, success := range []bool{true, false} {
		calls := 0
		r := &runner{path: filepath.Join(t.TempDir(), "report.json"), token: "synthetic-token", r: report{Account: strings.Repeat("a", 32), Buckets: []bucket{{Name: "aa-accept-confirmed", Created: true}, {Name: "aa-accept-uncertain", Created: false}}}}
		r.bucketCleanupClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != http.MethodDelete || req.URL.Host != "api.cloudflare.com" || !strings.HasSuffix(req.URL.Path, "/aa-accept-confirmed") || req.Header.Get("Authorization") != "Bearer synthetic-token" {
				t.Fatal("wrong cleanup target")
			}
			body := `{"success":false}`
			if success {
				body = `{"success":true}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		r.cleanup()
		if calls != 1 || r.r.Buckets[0].Deleted != success || r.r.Buckets[1].Deleted {
			t.Fatal("cleanup inferred unconfirmed deletion")
		}
	}
}

func TestErrorsNeverExposeProviderMessages(t *testing.T) {
	err := &cloudflare.Error{Status: 403, Codes: []int{1000}, Messages: []string{"sensitive-provider-canary"}}
	if strings.Contains(safeError(err), "canary") {
		t.Fatal("raw provider message escaped")
	}
}
