package cloudflare

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// API is what guided setup asks of Cloudflare. Client is the real
// implementation; setup's tests use it against an httptest server.
type API interface {
	// Accounts lists the accounts the token can see.
	Accounts(ctx context.Context) ([]Account, error)
	// CreateBucket makes a private R2 bucket.
	CreateBucket(ctx context.Context, account string, bucket BucketSpec) error
	// PermissionGroups lists the token permission groups called name.
	PermissionGroups(ctx context.Context, account, name string) ([]PermissionGroup, error)
	// CreateToken makes an account-owned API token. Its value is in the
	// answer only this once.
	CreateToken(ctx context.Context, account string, spec TokenSpec) (Token, error)
	// DeleteToken revokes an account-owned API token.
	DeleteToken(ctx context.Context, account, id string) error
	// ExpireObjectsAfter replaces the bucket's lifecycle rules with one
	// that deletes every object once it is age old.
	ExpireObjectsAfter(ctx context.Context, account string, bucket BucketRef, age time.Duration) error
	// ManagedDomain reports whether the bucket's public r2.dev URL is on.
	ManagedDomain(ctx context.Context, account string, bucket BucketRef) (ManagedDomain, error)
	// CustomDomains lists the custom domains attached to the bucket.
	CustomDomains(ctx context.Context, account string, bucket BucketRef) ([]CustomDomain, error)
	// Discard forgets the API token this API was made with.
	Discard()
}

// Account is a Cloudflare account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// BucketRef names a bucket, with the jurisdiction it lives in ("" for none).
type BucketRef struct {
	Name         string
	Jurisdiction string
}

// BucketSpec is a bucket to create.
type BucketSpec struct {
	BucketRef
	// LocationHint is a region hint ("" for automatic).
	LocationHint string
}

// PermissionGroup is a set of token permissions.
type PermissionGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// IsSelectable says whether the caller may put the group in a token.
	IsSelectable bool `json:"is_selectable"`
}

// Policy allows one permission group on some resources.
type Policy struct {
	PermissionGroupIDs []string
	// Resources maps a resource name to "*".
	Resources map[string]string
}

// TokenSpec is an account-owned API token to create. It never expires:
// expiry would silently end capture.
type TokenSpec struct {
	Name     string
	Policies []Policy
}

// Token is a created API token.
type Token struct {
	ID string
	// Value is the secret, returned by Cloudflare only when the token is
	// created. String hides it.
	Value string
}

// String describes the token without its secret.
func (t Token) String() string { return "token " + t.ID }

// GoString is String for %#v.
func (t Token) GoString() string { return t.String() }

// ManagedDomain is a bucket's r2.dev public URL.
type ManagedDomain struct {
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
}

// CustomDomain is a domain that serves a bucket publicly.
type CustomDomain struct {
	Domain  string `json:"domain"`
	Enabled bool   `json:"enabled"`
}

// Accounts lists the accounts the token can see, one page of up to 50.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var accounts []Account
	_, err := c.do(ctx, call{op: "list accounts", method: http.MethodGet, path: "/accounts", query: url.Values{"per_page": {"50"}}}, &accounts)
	return accounts, err
}

// CreateBucket makes the bucket.
func (c *Client) CreateBucket(ctx context.Context, account string, bucket BucketSpec) error {
	body := map[string]string{"name": bucket.Name}
	if bucket.LocationHint != "" {
		body["locationHint"] = bucket.LocationHint
	}
	_, err := c.do(ctx, call{op: "create bucket", method: http.MethodPost, path: accountPath(account) + "/r2/buckets", header: jurisdictionHeader(bucket.Jurisdiction), body: body}, nil)
	return err
}

// maxPermissionGroupPages bounds the paging loop below.
const maxPermissionGroupPages = 20

// PermissionGroups lists the groups named name, following every page.
func (c *Client) PermissionGroups(ctx context.Context, account, name string) ([]PermissionGroup, error) {
	var all []PermissionGroup
	seen := map[string]bool{}
	for page := 1; page <= maxPermissionGroupPages; page++ {
		var groups []PermissionGroup
		query := url.Values{"name": {name}, "page": {strconv.Itoa(page)}}
		env, err := c.do(ctx, call{op: "list token permission groups", method: http.MethodGet, path: accountPath(account) + "/tokens/permission_groups", query: query}, &groups)
		if err != nil {
			return nil, err
		}
		fresh := 0
		for _, g := range groups {
			if !seen[g.ID] {
				seen[g.ID] = true
				all = append(all, g)
				fresh++
			}
		}
		// Stop at the last page, and if a server that ignores the page
		// number sends the same groups again.
		if fresh == 0 || len(all) >= env.ResultInfo.TotalCount {
			break
		}
	}
	return all, nil
}

// tokenPolicy is the wire shape of Policy.
type tokenPolicy struct {
	Effect           string              `json:"effect"`
	PermissionGroups []map[string]string `json:"permission_groups"`
	Resources        map[string]string   `json:"resources"`
}

// CreateToken makes the token. No expires_on is sent.
func (c *Client) CreateToken(ctx context.Context, account string, spec TokenSpec) (Token, error) {
	const op = "create API token"
	body := struct {
		Name     string        `json:"name"`
		Policies []tokenPolicy `json:"policies"`
	}{Name: spec.Name}
	for _, p := range spec.Policies {
		policy := tokenPolicy{Effect: "allow", Resources: p.Resources}
		for _, id := range p.PermissionGroupIDs {
			policy.PermissionGroups = append(policy.PermissionGroups, map[string]string{"id": id})
		}
		body.Policies = append(body.Policies, policy)
	}
	var result struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if _, err := c.do(ctx, call{op: op, method: http.MethodPost, path: accountPath(account) + "/tokens", body: body}, &result); err != nil {
		return Token{}, err
	}
	if result.ID == "" || result.Value == "" {
		return Token{ID: result.ID}, &Error{Op: op, Status: http.StatusBadGateway, Err: errors.New("the answer had no token ID and value")}
	}
	return Token{ID: result.ID, Value: result.Value}, nil
}

// DeleteToken revokes the token.
func (c *Client) DeleteToken(ctx context.Context, account, id string) error {
	_, err := c.do(ctx, call{op: "revoke API token", method: http.MethodDelete, path: accountPath(account) + "/tokens/" + url.PathEscape(id)}, nil)
	return err
}

// retentionRuleID names the lifecycle rule setup writes.
const retentionRuleID = "agent-archive-retention"

// ExpireObjectsAfter writes the bucket's one lifecycle rule. The call
// replaces every rule the bucket has, so callers use it only on a bucket
// they just created.
func (c *Client) ExpireObjectsAfter(ctx context.Context, account string, bucket BucketRef, age time.Duration) error {
	body := map[string]any{"rules": []map[string]any{{
		"id":         retentionRuleID,
		"enabled":    true,
		"conditions": map[string]string{"prefix": ""},
		"deleteObjectsTransition": map[string]any{
			"condition": map[string]any{"type": "Age", "maxAge": int64(age / time.Second)},
		},
	}}}
	_, err := c.do(ctx, call{op: "set bucket lifecycle", method: http.MethodPut, path: bucketPath(account, bucket.Name) + "/lifecycle", header: jurisdictionHeader(bucket.Jurisdiction), body: body}, nil)
	return err
}

// ManagedDomain reads whether the r2.dev public URL is on.
func (c *Client) ManagedDomain(ctx context.Context, account string, bucket BucketRef) (ManagedDomain, error) {
	var domain ManagedDomain
	_, err := c.do(ctx, call{op: "read r2.dev public access", method: http.MethodGet, path: bucketPath(account, bucket.Name) + "/domains/managed", header: jurisdictionHeader(bucket.Jurisdiction)}, &domain)
	return domain, err
}

// CustomDomains lists the bucket's custom domains.
func (c *Client) CustomDomains(ctx context.Context, account string, bucket BucketRef) ([]CustomDomain, error) {
	var result struct {
		Domains []CustomDomain `json:"domains"`
	}
	_, err := c.do(ctx, call{op: "list custom domains", method: http.MethodGet, path: bucketPath(account, bucket.Name) + "/domains/custom", header: jurisdictionHeader(bucket.Jurisdiction)}, &result)
	return result.Domains, err
}
