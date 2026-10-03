// Package cloudflaretest is a fake of the part of Cloudflare's API that
// package cloudflare and guided R2 setup use, for tests. It keeps buckets,
// and tokens in memory, checks the bearer token, records
// every request, and can be told to fail any call. Test code only: depguard
// keeps it out of production code.
package cloudflaretest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Account is an account the fake lists.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Group is a permission group the fake lists.
type Group struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IsSelectable bool   `json:"is_selectable"`
}

// Issued is a token the fake created.
type Issued struct {
	ID    string
	Name  string
	Value string
	// Body is the request that created it.
	Body map[string]any
}

// Request is one request the fake received.
type Request struct {
	Route         Route
	Method        string
	Path          string
	Query         string
	Authorization string
	Jurisdiction  string
	Body          string
}

// Failure makes a route fail instead of doing its work.
type Failure struct {
	Status int
	// Message is the error message Cloudflare's JSON carries.
	Message string
	Code    int
	// RetryAfter, when set, is sent as the Retry-After header.
	RetryAfter string
	// AfterWork makes the call do its work before it fails, as when an
	// answer is lost on the way back.
	AfterWork bool
	// RawBody, when set, is sent as the whole body of the answer instead of
	// an error document, with Status.
	RawBody string
	// Times is how many calls fail; zero means every call.
	Times int
}

// Route names a call the fake serves.
type Route string

// Routes the fake serves, as Fail and Requests name them.
const (
	RouteAccounts         Route = "GET accounts"
	RouteListTokens       Route = "GET tokens"
	RouteTokenDetails     Route = "GET token"
	RouteCreateBucket     Route = "POST buckets"
	RoutePermissionGroups Route = "GET permission_groups"
	RouteCreateToken      Route = "POST tokens"
	RouteDeleteToken      Route = "DELETE token"
	RouteManagedDomain    Route = "GET managed"
	RouteCustomDomains    Route = "GET custom"
)

// Server is the fake. Set its exported fields before the code under test
// runs; read them after it has finished.
type Server struct {
	*httptest.Server
	// Token is the bearer token the fake accepts.
	Token string
	// Accounts is what GET /accounts lists.
	Accounts []Account
	// Groups is what the permission-group listing knows.
	Groups []Group
	// Buckets are the existing buckets, name to jurisdiction. A create for
	// a name in it fails as a name collision.
	Buckets map[string]string
	// ManagedEnabled is whether buckets' r2.dev public URL is on.
	ManagedEnabled bool
	// CustomDomains are the enabled custom domains every bucket lists.
	CustomDomains []string
	// ValuePrefix starts the value of every token the fake issues.
	ValuePrefix string
	// MetadataTokens extends the visible provider inventory with synthetic metadata.
	MetadataTokens []map[string]any

	mu       sync.Mutex
	tokens   []Issued
	revoked  map[string]bool
	requests []Request
	failures map[Route][]Failure
}

// AccountID is the account the fake lists by default.
const AccountID = "0123456789abcdef0123456789abcdef"

// New starts a fake that lists one account and the R2 bucket-item-write
// group, and accepts token.
func New(tb testing.TB, token string) *Server {
	tb.Helper()
	s := &Server{
		Token:    token,
		Accounts: []Account{{ID: AccountID, Name: "Test account"}},
		Groups: []Group{
			{ID: "aaaa0000000000000000000000000001", Name: "Workers R2 Storage Bucket Item Read", IsSelectable: true},
			{ID: "aaaa0000000000000000000000000002", Name: "Workers R2 Storage Bucket Item Write", IsSelectable: true},
		},
		Buckets:     map[string]string{},
		ValuePrefix: "issued-token-value",
		revoked:     map[string]bool{},
		failures:    map[Route][]Failure{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(s.Close)
	return s
}

// Fail makes calls to route fail as f says.
func (s *Server) Fail(route Route, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[route] = append(s.failures[route], f)
}

// SetPublicAccess turns the r2.dev public URL, and the custom domains the
// fake lists, on or off while the fake is serving, as someone changing them in
// the dashboard would. Set the fields before the fake serves, not after.
func (s *Server) SetPublicAccess(managed bool, customDomains []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ManagedEnabled, s.CustomDomains = managed, customDomains
}

// Requests returns every request received, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls counts the requests to route.
func (s *Server) Calls(route Route) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Route == route {
			n++
		}
	}
	return n
}

// Tokens returns the tokens issued so far.
func (s *Server) Tokens() []Issued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Issued(nil), s.tokens...)
}

// Live returns the tokens issued and not revoked.
func (s *Server) Live() []Issued {
	s.mu.Lock()
	defer s.mu.Unlock()
	var live []Issued
	for _, t := range s.tokens {
		if !s.revoked[t.ID] {
			live = append(live, t)
		}
	}
	return live
}

// routeTable maps a method and a path pattern to a route name; "*" matches
// one path segment.
var routeTable = []struct {
	method  string
	pattern string
	name    Route
}{
	{http.MethodGet, "accounts", RouteAccounts},
	{http.MethodPost, "accounts/*/r2/buckets", RouteCreateBucket},
	{http.MethodGet, "accounts/*/tokens/permission_groups", RoutePermissionGroups},
	{http.MethodPost, "accounts/*/tokens", RouteCreateToken},
	{http.MethodGet, "accounts/*/tokens", RouteListTokens},
	{http.MethodGet, "accounts/*/tokens/*", RouteTokenDetails},
	{http.MethodDelete, "accounts/*/tokens/*", RouteDeleteToken},
	{http.MethodGet, "accounts/*/r2/buckets/*/domains/managed", RouteManagedDomain},
	{http.MethodGet, "accounts/*/r2/buckets/*/domains/custom", RouteCustomDomains},
}

// route names the request, or "" for one the fake does not serve.
func route(method, path string) (name Route, parts []string) {
	parts = strings.Split(strings.Trim(path, "/"), "/")
	for _, entry := range routeTable {
		want := strings.Split(entry.pattern, "/")
		if entry.method != method || len(want) != len(parts) {
			continue
		}
		matched := true
		for i, segment := range want {
			matched = matched && (segment == "*" || segment == parts[i])
		}
		if matched {
			return entry.name, parts
		}
	}
	return "", parts
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	name, parts := route(r.Method, strings.TrimPrefix(r.URL.Path, "/client/v4"))
	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Route: name, Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Authorization: r.Header.Get("Authorization"), Jurisdiction: r.Header.Get("cf-r2-jurisdiction"), Body: string(body),
	})
	s.mu.Unlock()
	if name == "" {
		writeError(w, http.StatusNotFound, 7003, "no route for "+r.Method+" "+r.URL.Path, "")
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeError(w, http.StatusUnauthorized, 10000, "Authentication error", "")
		return
	}
	if f, ok := s.nextFailure(name); ok {
		if f.AfterWork {
			s.work(httptest.NewRecorder(), r, name, parts, body)
		}
		message := f.Message
		if message == "" {
			message = "injected failure"
		}
		if f.RawBody != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.Status)
			_, _ = w.Write([]byte(f.RawBody))
			return
		}
		writeError(w, f.Status, f.Code, message, f.RetryAfter)
		return
	}
	s.work(w, r, name, parts, body)
}

// work does what route name asks, and writes the answer.
func (s *Server) work(w http.ResponseWriter, r *http.Request, name Route, parts []string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account := ""
	if len(parts) > 1 {
		account = parts[1]
	}
	if name != RouteAccounts && !s.hasAccount(account) {
		writeError(w, http.StatusNotFound, 7003, "unknown account", "")
		return
	}
	switch name {
	case RouteAccounts:
		accounts := append([]Account{}, s.Accounts...)
		writeResult(w, accounts, len(accounts))
	case RouteCreateBucket:
		s.createBucket(w, r, body)
	case RoutePermissionGroups:
		out := []Group{}
		for _, g := range s.Groups {
			if want := r.URL.Query().Get("name"); want == "" || g.Name == want {
				out = append(out, g)
			}
		}
		writeResult(w, out, len(out))
	case RouteListTokens:
		all := s.metadataTokens()
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 || size < 5 || size > 50 || r.URL.Query().Get("include_expired") != "true" {
			writeError(w, http.StatusBadRequest, 10001, "invalid token pagination", "")
			return
		}
		start := min((page-1)*size, len(all))
		end := min(start+size, len(all))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": all[start:end], "result_info": map[string]int{"page": page, "per_page": size, "count": end - start, "total_count": len(all)}})
	case RouteTokenDetails:
		for _, token := range s.metadataTokens() {
			if token["id"] == parts[3] {
				writeResult(w, token, 0)
				return
			}
		}
		writeError(w, http.StatusNotFound, 1003, "token not found", "")
	case RouteCreateToken:
		s.createToken(w, body)
	case RouteDeleteToken:
		id := parts[3]
		found := false
		for _, t := range s.tokens {
			found = found || t.ID == id
		}
		if !found || s.revoked[id] {
			writeError(w, http.StatusNotFound, 1003, "token not found", "")
			return
		}
		s.revoked[id] = true
		writeResult(w, map[string]string{"id": id}, 0)
	case RouteManagedDomain:
		writeResult(w, map[string]any{"bucketId": "id", "domain": "pub-x.r2.dev", "enabled": s.ManagedEnabled}, 0)
	case RouteCustomDomains:
		domains := []map[string]any{}
		for _, d := range s.CustomDomains {
			domains = append(domains, map[string]any{"domain": d, "enabled": true})
		}
		writeResult(w, map[string]any{"domains": domains}, 0)
	}
}

func (s *Server) hasAccount(id string) bool {
	for _, a := range s.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func (s *Server) nextFailure(route Route) (Failure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.failures[route]
	if len(queue) == 0 {
		return Failure{}, false
	}
	f := queue[0]
	if f.Times > 0 {
		f.Times--
		if f.Times == 0 {
			s.failures[route] = queue[1:]
		} else {
			queue[0] = f
		}
	}
	return f, true
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Name         string `json:"name"`
		LocationHint string `json:"locationHint"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, 10001, "bad body", "")
		return
	}
	if _, exists := s.Buckets[req.Name]; exists {
		writeError(w, http.StatusConflict, 10073, "Bucket name already exists.", "")
		return
	}
	s.Buckets[req.Name] = r.Header.Get("cf-r2-jurisdiction")
	writeResult(w, map[string]string{"name": req.Name, "location": strings.ToUpper(req.LocationHint)}, 0)
}

func (s *Server) createToken(w http.ResponseWriter, body []byte) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, 10001, "bad body", "")
		return
	}
	n := len(s.tokens) + 1
	name, _ := req["name"].(string)
	if len(name) > 120 {
		writeError(w, http.StatusBadRequest, 10001, "token name exceeds 120 characters", "")
		return
	}
	issued := Issued{ID: fmt.Sprintf("%032x", n), Name: name, Value: fmt.Sprintf("%s-%d", s.ValuePrefix, n), Body: req}
	s.tokens = append(s.tokens, issued)
	writeResult(w, map[string]any{"id": issued.ID, "value": issued.Value, "name": issued.Name, "status": "active"}, 0)
}

func writeResult(w http.ResponseWriter, result any, total int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": result,
		"result_info": map[string]int{"page": 1, "per_page": 50, "count": total, "total_count": total},
	})
}

func writeError(w http.ResponseWriter, status, code int, message, retryAfter string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false, "errors": []map[string]any{{"code": code, "message": message}}, "messages": []any{}, "result": nil,
	})
}

func (s *Server) metadataTokens() []map[string]any {
	all := append([]map[string]any{}, s.MetadataTokens...)
	for _, token := range s.tokens {
		status := "active"
		if s.revoked[token.ID] {
			status = "disabled"
		}
		all = append(all, map[string]any{"id": token.ID, "name": token.Name, "status": status, "policies": token.Body["policies"]})
	}
	return all
}
