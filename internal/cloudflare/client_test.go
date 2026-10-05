package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

const canary = "CANARY-bootstrap-token-value"

type sleeps struct{ waits []time.Duration }

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return nil
}

func newClient(t *testing.T) (*cloudflare.Client, *cloudflaretest.Server, *sleeps) {
	t.Helper()
	srv := cloudflaretest.New(t, canary)
	s := &sleeps{}
	client := cloudflare.New(canary, cloudflare.Options{BaseURL: srv.URL + "/client/v4", Sleep: s.sleep})
	t.Cleanup(client.Discard)
	return client, srv, s
}

func apiError(t *testing.T, err error) *cloudflare.Error {
	t.Helper()
	var apiErr *cloudflare.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *cloudflare.Error", err)
	}
	return apiErr
}

func TestClientListsAccountsWithBearerAuth(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	accounts, err := client.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].ID != cloudflaretest.AccountID || accounts[0].Name != "Test account" {
		t.Fatalf("accounts %+v, %v", accounts, err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Authorization != "Bearer "+canary || reqs[0].Path != "/client/v4/accounts" {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestClientCreatesABucketWithJurisdictionAndLocationHint(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	spec := cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: "my-bucket", Jurisdiction: "eu"}, LocationHint: "weur"}
	if err := client.CreateBucket(context.Background(), cloudflaretest.AccountID, spec); err != nil {
		t.Fatal(err)
	}
	req := srv.Requests()[0]
	if req.Route != cloudflaretest.RouteCreateBucket || req.Jurisdiction != "eu" {
		t.Fatalf("request %+v", req)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil || body["name"] != "my-bucket" || body["locationHint"] != "weur" || len(body) != 2 {
		t.Fatalf("body %s (%v)", req.Body, err)
	}
	// No jurisdiction and no hint send neither.
	if err := client.CreateBucket(context.Background(), cloudflaretest.AccountID, cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: "plain-bucket"}}); err != nil {
		t.Fatal(err)
	}
	plain := srv.Requests()[1]
	if plain.Jurisdiction != "" || strings.Contains(plain.Body, "locationHint") {
		t.Fatalf("plain request %+v", plain)
	}
}

func TestClientBucketNameCollisionIsAlreadyExists(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	srv.Buckets["taken-name"] = ""
	err := client.CreateBucket(context.Background(), cloudflaretest.AccountID, cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: "taken-name"}})
	apiErr := apiError(t, err)
	if !apiErr.AlreadyExists() || apiErr.Status != http.StatusConflict || len(apiErr.Codes) != 1 || apiErr.Codes[0] != 10004 {
		t.Fatalf("error %+v", apiErr)
	}
	// Both the observed REST code and documented Workers/S3 code need 409.
	for _, code := range []int{10004, 10073} {
		if !(&cloudflare.Error{Status: http.StatusConflict, Codes: []int{code}}).AlreadyExists() {
			t.Errorf("bucket conflict code %d was refused", code)
		}
	}
	for name, other := range map[string]*cloudflare.Error{
		"bare conflict":      {Status: http.StatusConflict},
		"message only":       {Status: http.StatusConflict, Messages: []string{"The bucket already exists."}},
		"other code":         {Status: http.StatusConflict, Codes: []int{10008}},
		"REST code, bad 400": {Status: http.StatusBadRequest, Codes: []int{10004}},
		"code, bad 400":      {Status: http.StatusBadRequest, Codes: []int{10073}},
		"message at 400":     {Status: http.StatusBadRequest, Messages: []string{"Bucket name already exists."}},
	} {
		if other.AlreadyExists() {
			t.Errorf("%s counts as a name collision", name)
		}
	}
}

func TestClientFindsPermissionGroupsByName(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	groups, err := client.PermissionGroups(context.Background(), cloudflaretest.AccountID, cloudflare.PermissionBucketItemWrite)
	if err != nil || len(groups) != 1 || groups[0].Name != cloudflare.PermissionBucketItemWrite || !groups[0].IsSelectable {
		t.Fatalf("groups %+v, %v", groups, err)
	}
	if q := srv.Requests()[0].Query; !strings.Contains(q, "name=Workers+R2+Storage+Bucket+Item+Write") {
		t.Fatalf("query %q", q)
	}
	id, err := cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite)
	if err != nil || id != "aaaa0000000000000000000000000002" {
		t.Fatalf("id %q, %v", id, err)
	}
}

func TestSelectPermissionGroupRequiresASelectableGroup(t *testing.T) {
	t.Parallel()
	const name = cloudflare.PermissionBucketItemWrite
	if _, err := cloudflare.SelectPermissionGroup(nil, name); err == nil || !strings.Contains(err.Error(), "no") {
		t.Fatalf("missing group: %v", err)
	}
	locked := []cloudflare.PermissionGroup{{ID: "x", Name: name, IsSelectable: false}}
	if _, err := cloudflare.SelectPermissionGroup(locked, name); err == nil || !strings.Contains(err.Error(), "may not grant") {
		t.Fatalf("unselectable group: %v", err)
	}
	other := []cloudflare.PermissionGroup{{ID: "x", Name: "Other", IsSelectable: true}}
	if _, err := cloudflare.SelectPermissionGroup(other, name); err == nil {
		t.Fatal("a group with another name was accepted")
	}
	mixed := []cloudflare.PermissionGroup{{ID: "a", Name: name}, {ID: "b", Name: name, IsSelectable: true}}
	if id, err := cloudflare.SelectPermissionGroup(mixed, name); err != nil || id != "b" {
		t.Fatalf("mixed: %q %v", id, err)
	}
}

// A server that ignores the page number and repeats itself ends the paging.
func TestClientPermissionGroupPagingStopsOnRepeats(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"g1","name":"n","is_selectable":true}],"result_info":{"total_count":5}}`))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	groups, err := client.PermissionGroups(context.Background(), "acct", "n")
	if err != nil || len(groups) != 1 || calls != 2 {
		t.Fatalf("groups %+v, %d calls, %v", groups, calls, err)
	}
}

// An answer without result_info (no total) does not end paging: the group
// asked for may be on the next page. Paging then ends when a page adds
// nothing.
func TestClientPermissionGroupPagingWithoutATotalFollowsThePages(t *testing.T) {
	t.Parallel()
	pages := map[string]string{
		"1": `{"success":true,"result":[{"id":"g1","name":"other","is_selectable":true}]}`,
		"2": `{"success":true,"result":[{"id":"g2","name":"target","is_selectable":true}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Query().Get("page")]
		if !ok {
			body = `{"success":true,"result":[]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	groups, err := client.PermissionGroups(context.Background(), "acct", "target")
	if err != nil {
		t.Fatal(err)
	}
	id, err := cloudflare.SelectPermissionGroup(groups, "target")
	if err != nil || id != "g2" {
		t.Fatalf("groups %+v: %q, %v", groups, id, err)
	}
}

// A server that ignores the page number and sends no total still ends paging.
func TestClientPermissionGroupPagingWithoutATotalStopsOnRepeats(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"g1","name":"n","is_selectable":true}]}`))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	groups, err := client.PermissionGroups(context.Background(), "acct", "n")
	if err != nil || len(groups) != 1 || calls.Load() != 2 {
		t.Fatalf("groups %+v, %d calls, %v", groups, calls.Load(), err)
	}
}

func TestClientCreatesATokenWithOneAllowPolicyAndNoExpiry(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	resource, err := cloudflare.BucketResource(cloudflaretest.AccountID, cloudflare.BucketRef{Name: "b-1"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := client.CreateToken(context.Background(), cloudflaretest.AccountID, cloudflare.TokenSpec{
		Name:     "agent-archive b-1 abc123",
		Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{"gid"}, Resources: map[string]string{resource: "*"}}},
	})
	if err != nil || token.ID == "" || token.Value == "" {
		t.Fatalf("token %v, %v", token, err)
	}
	body := srv.Requests()[0].Body
	var got struct {
		Name     string `json:"name"`
		Policies []struct {
			Effect           string              `json:"effect"`
			PermissionGroups []map[string]string `json:"permission_groups"`
			Resources        map[string]string   `json:"resources"`
		} `json:"policies"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "expires_on") || strings.Contains(body, "not_before") {
		t.Fatalf("token body sets a time limit: %s", body)
	}
	if got.Name != "agent-archive b-1 abc123" || len(got.Policies) != 1 {
		t.Fatalf("body %s", body)
	}
	p := got.Policies[0]
	if p.Effect != "allow" || len(p.PermissionGroups) != 1 || p.PermissionGroups[0]["id"] != "gid" || p.Resources[resource] != "*" || len(p.Resources) != 1 {
		t.Fatalf("policy %+v", p)
	}
	if !strings.HasPrefix(resource, "com.cloudflare.edge.r2.bucket."+cloudflaretest.AccountID+"_default_b-1") {
		t.Fatalf("resource %q", resource)
	}
}

func TestTokenValueIsHiddenFromFormatting(t *testing.T) {
	t.Parallel()
	token := cloudflare.Token{ID: "abc", Value: "SECRET-VALUE"}
	for _, text := range []string{token.String(), fmt.Sprintf("%v", token), fmt.Sprintf("%+v", token), fmt.Sprintf("%#v", token), fmt.Sprintf("%v", &token)} {
		if strings.Contains(text, "SECRET-VALUE") {
			t.Fatalf("formatting shows the value: %s", text)
		}
	}
}

func TestBucketResourceUsesTheJurisdiction(t *testing.T) {
	t.Parallel()
	got, err := cloudflare.BucketResource("acct", cloudflare.BucketRef{Name: "bkt", Jurisdiction: "eu"})
	if err != nil || got != "com.cloudflare.edge.r2.bucket.acct_eu_bkt" {
		t.Fatalf("resource %q, %v", got, err)
	}
	for _, tc := range []struct {
		account string
		bucket  string
	}{{"", "bkt"}, {"acct", ""}, {"", ""}} {
		if got, err := cloudflare.BucketResource(tc.account, cloudflare.BucketRef{Name: tc.bucket}); err == nil {
			t.Errorf("account %q, bucket %q gave resource %q", tc.account, tc.bucket, got)
		}
	}
	if got := cloudflare.Endpoint("acct", "eu"); got != "https://acct.eu.r2.cloudflarestorage.com" {
		t.Fatalf("endpoint %q", got)
	}
	if got := cloudflare.Endpoint("acct", ""); got != "https://acct.r2.cloudflarestorage.com" {
		t.Fatalf("endpoint %q", got)
	}
}

func TestClientRefusesATokenAnswerWithoutAValue(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"abc"}}`))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	token, err := client.CreateToken(context.Background(), "acct", cloudflare.TokenSpec{Name: "n"})
	if err == nil || token.ID != "abc" {
		t.Fatalf("token %v, %v", token, err)
	}
}

func TestClientRevokesAToken(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	token, err := client.CreateToken(context.Background(), cloudflaretest.AccountID, cloudflare.TokenSpec{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteToken(context.Background(), cloudflaretest.AccountID, token.ID); err != nil {
		t.Fatal(err)
	}
	if len(srv.Live()) != 0 {
		t.Fatalf("token still live: %+v", srv.Live())
	}
	if last := srv.Requests()[1]; last.Method != http.MethodDelete || last.Path != "/client/v4/accounts/"+cloudflaretest.AccountID+"/tokens/"+token.ID {
		t.Fatalf("request %+v", last)
	}
	if err := client.DeleteToken(context.Background(), cloudflaretest.AccountID, token.ID); !apiError(t, err).NotFound() {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestClientReadsPublicAccessSettings(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	ref := cloudflare.BucketRef{Name: "some-bucket"}
	domain, err := client.ManagedDomain(context.Background(), cloudflaretest.AccountID, ref)
	if err != nil || domain.Enabled || domain.Domain == "" {
		t.Fatalf("managed %+v, %v", domain, err)
	}
	srv.ManagedEnabled = true
	srv.CustomDomains = []string{"files.example.com"}
	if domain, err = client.ManagedDomain(context.Background(), cloudflaretest.AccountID, ref); err != nil || !domain.Enabled {
		t.Fatalf("managed %+v, %v", domain, err)
	}
	custom, err := client.CustomDomains(context.Background(), cloudflaretest.AccountID, ref)
	if err != nil || len(custom) != 1 || custom[0].Domain != "files.example.com" || !custom[0].Enabled {
		t.Fatalf("custom %+v, %v", custom, err)
	}
}

func TestClientClassifiesFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		check  func(*cloudflare.Error) bool
	}{
		{http.StatusUnauthorized, (*cloudflare.Error).Unauthorized},
		{http.StatusForbidden, (*cloudflare.Error).Forbidden},
		{http.StatusNotFound, (*cloudflare.Error).NotFound},
		{http.StatusInternalServerError, (*cloudflare.Error).ServerError},
		{http.StatusBadGateway, (*cloudflare.Error).ServerError},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			client, srv, _ := newClient(t)
			srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: tc.status, Code: 1234, Message: "no thanks"})
			_, err := client.Accounts(context.Background())
			apiErr := apiError(t, err)
			if !tc.check(apiErr) || apiErr.Status != tc.status || len(apiErr.Codes) != 1 || apiErr.Codes[0] != 1234 || len(apiErr.Messages) != 1 || apiErr.Messages[0] != "no thanks" {
				t.Fatalf("error %+v", apiErr)
			}
			if !strings.Contains(err.Error(), "no thanks") || !strings.Contains(err.Error(), "list accounts") {
				t.Fatalf("message %q", err)
			}
		})
	}
}

func TestClientWrongTokenIsUnauthorized(t *testing.T) {
	t.Parallel()
	srv := cloudflaretest.New(t, "the-right-token")
	client := cloudflare.New("a-wrong-token", cloudflare.Options{BaseURL: srv.URL + "/client/v4"})
	_, err := client.Accounts(context.Background())
	if apiErr := apiError(t, err); !apiErr.Unauthorized() {
		t.Fatalf("error %+v", apiErr)
	}
}

func TestClientHonorsRetryAfterOnRateLimit(t *testing.T) {
	t.Parallel()
	client, srv, s := newClient(t)
	srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "7", Times: 2})
	accounts, err := client.Accounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts %+v, %v", accounts, err)
	}
	if len(s.waits) != 2 || s.waits[0] != 7*time.Second || s.waits[1] != 7*time.Second {
		t.Fatalf("waits %v", s.waits)
	}
	if srv.Calls(cloudflaretest.RouteAccounts) != 3 {
		t.Fatalf("calls %d", srv.Calls(cloudflaretest.RouteAccounts))
	}
}

func TestClientRateLimitWithoutRetryAfterBacksOff(t *testing.T) {
	t.Parallel()
	client, srv, s := newClient(t)
	srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusTooManyRequests, Times: 2})
	if _, err := client.Accounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.waits) != 2 || s.waits[0] != 2*time.Second || s.waits[1] != 4*time.Second {
		t.Fatalf("waits %v", s.waits)
	}
}

func TestClientGivesUpOnALongRetryAfter(t *testing.T) {
	t.Parallel()
	client, srv, s := newClient(t)
	srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "600"})
	_, err := client.Accounts(context.Background())
	apiErr := apiError(t, err)
	if !apiErr.RateLimited() || apiErr.RetryAfter != 10*time.Minute || len(s.waits) != 0 {
		t.Fatalf("error %+v, waits %v", apiErr, s.waits)
	}
}

func TestClientRateLimitRetriesAreBounded(t *testing.T) {
	t.Parallel()
	client, srv, s := newClient(t)
	srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "1"})
	_, err := client.Accounts(context.Background())
	if !apiError(t, err).RateLimited() || len(s.waits) != 3 || srv.Calls(cloudflaretest.RouteAccounts) != 4 {
		t.Fatalf("waits %v, calls %d, %v", s.waits, srv.Calls(cloudflaretest.RouteAccounts), err)
	}
}

func TestClientStopsWaitingWhenTheSleepFails(t *testing.T) {
	t.Parallel()
	srv := cloudflaretest.New(t, "tok")
	srv.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "1"})
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL + "/client/v4", Sleep: func(context.Context, time.Duration) error { return context.Canceled }})
	_, err := client.Accounts(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

func TestClientErrorsNeverCarryTheToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A server that echoes what it was sent, as a misbehaving proxy might.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"bad header ` + r.Header.Get("Authorization") + `"}]}`))
	}))
	defer srv.Close()
	client := cloudflare.New(canary, cloudflare.Options{BaseURL: srv.URL})
	_, err := client.Accounts(context.Background())
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error %v", err)
	}
}

func TestClientTransportErrorsNeverCarryTheToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	client := cloudflare.New(canary, cloudflare.Options{BaseURL: base + "/" + canary})
	_, err := client.Accounts(context.Background())
	apiErr := apiError(t, err)
	if apiErr.Status != 0 || strings.Contains(err.Error(), canary) {
		t.Fatalf("error %v", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	seen := ""
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer srv.Close()
	client := cloudflare.New(canary, cloudflare.Options{BaseURL: srv.URL})
	_, err := client.Accounts(context.Background())
	if apiErr := apiError(t, err); apiErr.Status != http.StatusFound || seen != "" {
		t.Fatalf("error %+v, the other host saw %q", apiErr, seen)
	}
}

func TestClientRejectsAnswersThatAreNotJSON(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"html error page":      {http.StatusBadGateway, "<html>bad gateway</html>"},
		"html success":         {http.StatusOK, "<html>hello</html>"},
		"success false on 200": {http.StatusOK, `{"success":false,"errors":[{"code":1,"message":"nope"}]}`},
		"wrong result shape":   {http.StatusOK, `{"success":true,"result":"text"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
			if _, err := client.Accounts(context.Background()); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestDiscardedClientSendsNothing(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	client.Discard()
	if _, err := client.Accounts(context.Background()); err == nil || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("error %v", err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests after Discard", n)
	}
}

func TestClientEscapesPathParts(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	_ = client.DeleteToken(context.Background(), "acct/../x", "id?y")
	if p := srv.Requests()[0].Path; strings.Contains(p, "/../") || !strings.Contains(p, "acct%2F..%2Fx") || !strings.Contains(p, "id%3Fy") {
		t.Fatalf("path %q", p)
	}
}

func TestDeriveS3CredentialsTestVector(t *testing.T) {
	t.Parallel()
	// SHA-256 of "abc" is a published test vector. This pins the encoding
	// implemented (lowercase hex), not Cloudflare's: see the function's
	// comment and the live acceptance checklist.
	got := cloudflare.DeriveS3Credentials(cloudflare.Token{ID: "0123456789abcdef0123456789abcdef", Value: "abc"})
	if got.AccessKeyID != "0123456789abcdef0123456789abcdef" || got.SecretAccessKey != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("credentials %+v", got)
	}
}

func TestValidateBucketName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"abc", "agent-archive-1a2b3c", "a1b", strings.Repeat("a", 63), "1-2-3"} {
		if err := cloudflare.ValidateBucketName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("a", 64), "-abc", "abc-", "Abc", "a_b", "a b", "a.b", "ab!"} {
		if err := cloudflare.ValidateBucketName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestJurisdictionsAndHintsAreThoseCloudflareDocuments(t *testing.T) {
	t.Parallel()
	for _, j := range []string{"eu", "us", "fedramp"} {
		if !cloudflare.ValidJurisdiction(j) {
			t.Errorf("jurisdiction %q rejected", j)
		}
	}
	for _, h := range []string{"apac", "eeur", "enam", "weur", "wnam", "oc"} {
		if !cloudflare.ValidLocationHint(h) {
			t.Errorf("hint %q rejected", h)
		}
	}
	// fedramp-high is documented only for the create header, so it is not
	// offered until its resource string and endpoint are confirmed live.
	for _, bad := range []string{"default", "", "fedramp-high", "EU"} {
		if cloudflare.ValidJurisdiction(bad) {
			t.Errorf("jurisdiction %q accepted", bad)
		}
	}
	if cloudflare.ValidLocationHint("mars") {
		t.Fatal("an invalid hint was accepted")
	}
}

func TestS3CredentialsHideTheSecretFromFormatting(t *testing.T) {
	t.Parallel()
	creds := cloudflare.S3Credentials{AccessKeyID: "the-id", SecretAccessKey: "SECRET-HASH"}
	for _, text := range []string{creds.String(), fmt.Sprintf("%v", creds), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds), fmt.Sprintf("%v", &creds)} {
		if strings.Contains(text, "SECRET-HASH") {
			t.Fatalf("formatting shows the secret: %s", text)
		}
	}
}

// An answer larger than the cap is cut off, so a valid document that only
// starts after the cap is never read.
func TestClientReadsAtMostAMegabyteOfAnAnswer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20)))
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"a","name":"n"}]}`))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	accounts, err := client.Accounts(context.Background())
	if err == nil || len(accounts) != 0 {
		t.Fatalf("accounts %+v, %v", accounts, err)
	}
}

func TestClientAnswersItCannotReadSayWhy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()
	client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
	_, err := client.Accounts(context.Background())
	apiErr := apiError(t, err)
	if apiErr.Status != http.StatusOK || apiErr.Err == nil || !strings.Contains(err.Error(), "could not be read as JSON") {
		t.Fatalf("error %+v", apiErr)
	}
}

// A success that carries nothing to read is unreadable, not a negative: no
// accounts, no permission groups, "r2.dev off", or "no custom domains" may be
// concluded from it.
func TestClientEmptySuccessfulAnswersAreUnreadableNotNegative(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"no result":   `{"success":true}`,
		"null result": `{"success":true,"result":null}`,
		"empty":       `{"success":true,"result":{}}`,
	}
	account, bucket := cloudflaretest.AccountID, cloudflare.BucketRef{Name: "my-bucket"}
	calls := map[string]func(*cloudflare.Client) error{
		"accounts": func(c *cloudflare.Client) error { _, err := c.Accounts(context.Background()); return err },
		"permission groups": func(c *cloudflare.Client) error {
			_, err := c.PermissionGroups(context.Background(), account, "x")
			return err
		},
		"managed domain": func(c *cloudflare.Client) error {
			_, err := c.ManagedDomain(context.Background(), account, bucket)
			return err
		},
		"custom domains": func(c *cloudflare.Client) error {
			_, err := c.CustomDomains(context.Background(), account, bucket)
			return err
		},
	}
	for bodyName, body := range bodies {
		for callName, call := range calls {
			t.Run(bodyName+"/"+callName, func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()
				client := cloudflare.New("tok", cloudflare.Options{BaseURL: srv.URL})
				apiErr := apiError(t, call(client))
				if apiErr.Err == nil || apiErr.Status/100 != 2 && apiErr.Status != http.StatusBadGateway {
					t.Fatalf("error %+v is not an unreadable answer", apiErr)
				}
			})
		}
	}
}
