package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// InventoryAPI reads account-owned token metadata without ever reading token values.
type InventoryAPI interface {
	TokenInventory(context.Context, string) (TokenInventory, error)
	TokenDetails(context.Context, string, string) (TokenMetadata, error)
}

// MetadataPolicy holds provider policy evidence. Nested resource shapes remain unknown.
type MetadataPolicy struct {
	Effect           string                    `json:"effect"`
	PermissionGroups []MetadataPermissionGroup `json:"permission_groups"`
	Resources        map[string]string         `json:"resources"`
	Unsupported      bool                      `json:"-"`
}

// MetadataPermissionGroup retains only the public policy identifier.
type MetadataPermissionGroup struct {
	ID string `json:"id"`
}

type metadataPolicyField string

const (
	metadataPolicyID        metadataPolicyField = "id"
	metadataPolicyEffect    metadataPolicyField = "effect"
	metadataPolicyGroups    metadataPolicyField = "permission_groups"
	metadataPolicyResources metadataPolicyField = "resources"
)

// UnmarshalJSON preserves supported simple resource maps while classifying
// nested/unknown values without retaining their arbitrary contents.
func (p *MetadataPolicy) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Effect           string                    `json:"effect"`
		PermissionGroups []MetadataPermissionGroup `json:"permission_groups"`
		Resources        json.RawMessage           `json:"resources"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	p.Effect = wire.Effect
	p.PermissionGroups = wire.PermissionGroups
	resources, supported := simplePolicyResources(wire.Resources)
	p.Resources = resources
	p.Unsupported = !supported
	var fields map[metadataPolicyField]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		switch key {
		case metadataPolicyID, metadataPolicyEffect, metadataPolicyGroups, metadataPolicyResources:
		default:
			p.Unsupported = true
		}
	}
	return nil
}

func simplePolicyResources(raw []byte) (map[string]string, bool) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || len(values) > 32 {
		return nil, false
	}
	resources := map[string]string{}
	for key, value := range values {
		var permission string
		if err := json.Unmarshal(value, &permission); err != nil || len(key) > 256 || permission != "*" {
			return nil, false
		}
		resources[key] = permission
	}
	return resources, true
}

// TokenStatus is a provider-reported state, not proof that data-plane access works.
type TokenStatus string

// Documented Cloudflare account token states.
const (
	TokenStatusActive   TokenStatus = "active"
	TokenStatusDisabled TokenStatus = "disabled"
	TokenStatusExpired  TokenStatus = "expired"
)

// TokenMetadata deliberately excludes token values, creator emails and other personal data.
type TokenMetadata struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Status    TokenStatus      `json:"status"`
	Policies  []MetadataPolicy `json:"policies"`
	ExpiresOn string           `json:"expires_on,omitempty"`
	NotBefore string           `json:"not_before,omitempty"`
}

// TokenInventory separates completed pagination from account-wide visibility.
// Cloudflare may return only tokens created by a list_self caller; the response
// does not attest to broader permission. Absence never proves revocation.
type TokenInventory struct {
	Tokens             []TokenMetadata
	PaginationComplete bool
	Visibility         string
}

// InventoryTimeout bounds one explicit provider inventory operation.
const InventoryTimeout = 20 * time.Second

// TokenInventory reads at most 20 pages and 1,000 tokens under one deadline.
// Request/response shapes follow Cloudflare's account token list API, checked
// 2026-10-01: page is numeric and per_page is between five and fifty.
func (c *Client) TokenInventory(ctx context.Context, account string) (TokenInventory, error) {
	result := TokenInventory{Visibility: "unknown_may_be_creator_only"}
	if err := validateProviderID(account); err != nil {
		return result, errors.New("invalid account identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, InventoryTimeout)
	defer cancel()
	seen := map[string]bool{}
	total := -1
	for page := 1; page <= 20; page++ {
		var tokens []TokenMetadata
		env, err := c.do(ctx, call{op: "list account token metadata", method: http.MethodGet, path: accountPath(account) + "/tokens", query: url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}, "include_expired": {"true"}}}, &tokens)
		if err != nil {
			return result, err
		}
		if total == -1 {
			total = env.ResultInfo.TotalCount
		}
		if !env.ResultInfo.countsPresent || env.ResultInfo.Page != page || env.ResultInfo.PerPage != 50 || len(tokens) > env.ResultInfo.PerPage || env.ResultInfo.Count != len(tokens) || env.ResultInfo.TotalCount != total || total < len(result.Tokens)+len(tokens) || (len(result.Tokens)+len(tokens) < total && len(tokens) != env.ResultInfo.PerPage) {
			return result, errors.New("token pagination evidence is incomplete")
		}
		for _, token := range tokens {
			if !validTokenMetadata(token) || c.metadataContainsToken(token) || seen[token.ID] {
				return result, errors.New("invalid or repeated token metadata")
			}
			seen[token.ID] = true
			result.Tokens = append(result.Tokens, token)
		}
		if len(result.Tokens) == env.ResultInfo.TotalCount {
			result.PaginationComplete = true
			return result, nil
		}
		if len(tokens) == 0 {
			return result, errors.New("token pagination ended before its reported total")
		}
	}
	return result, errors.New("token inventory exceeded its page limit")
}

// TokenDetails reads one account-owned token's metadata; a 404 is not proof of deletion.
func (c *Client) TokenDetails(ctx context.Context, account, id string) (TokenMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, InventoryTimeout)
	defer cancel()
	var result TokenMetadata
	if validateProviderID(account) != nil || validateProviderID(id) != nil {
		return result, errors.New("invalid account or token identifier")
	}
	_, err := c.do(ctx, call{op: "read account token metadata", method: http.MethodGet, path: accountPath(account) + "/tokens/" + url.PathEscape(id)}, &result)
	if err != nil {
		return TokenMetadata{}, err
	}
	if result.ID != id || !validTokenMetadata(result) || c.metadataContainsToken(result) {
		return TokenMetadata{}, errors.New("invalid token detail metadata")
	}
	return result, nil
}

func validTokenMetadata(t TokenMetadata) bool {
	if validateProviderID(t.ID) != nil || len(t.Name) > 120 || len(t.Policies) > 16 {
		return false
	}
	for _, p := range t.Policies {
		if len(p.PermissionGroups) > 32 || len(p.Resources) > 32 {
			return false
		}
	}
	return true
}

// ExactBucketPolicy verifies only the supported object-write permission on one
// exact account/jurisdiction/bucket resource. Broader, denied, nested or unknown
// policies never establish this binding.
func ExactBucketPolicy(token TokenMetadata, account string, bucket BucketRef, permissionID string) bool {
	resource, err := BucketResource(account, bucket)
	if err != nil || permissionID == "" || len(token.Policies) != 1 {
		return false
	}
	p := token.Policies[0]
	if p.Unsupported || p.Effect != "allow" || len(p.PermissionGroups) != 1 || p.PermissionGroups[0].ID != permissionID {
		return false
	}
	resources := p.Resources
	if len(resources) != 1 {
		return false
	}
	return resources[resource] == "*"
}

func validateProviderID(id string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return errors.New("invalid provider identifier")
	}
	return nil
}

// ProviderIdentity names immutable issuance identifiers; it does not bind a machine.
type ProviderIdentity struct {
	RecipientID string
	IssuerID    string
	SlotID      string
}

// ParseProviderName recognizes the canonical 118-byte provider name. The bucket
// is intentionally absent: Cloudflare limits names to 120 bytes, and policies
// alone establish destination scope. Oversized older proposal names are refused.
func ParseProviderName(name string) (ProviderIdentity, bool) {
	var id ProviderIdentity
	if len(name) != 118 {
		return id, false
	}
	parts := strings.Split(name, " ")
	if len(parts) != 4 || parts[0] != "agent-archive" || !strings.HasPrefix(parts[1], "r=") || !strings.HasPrefix(parts[2], "i=") || !strings.HasPrefix(parts[3], "k=") {
		return id, false
	}
	id = ProviderIdentity{RecipientID: strings.TrimPrefix(parts[1], "r="), IssuerID: strings.TrimPrefix(parts[2], "i="), SlotID: strings.TrimPrefix(parts[3], "k=")}
	if validateProviderID(id.RecipientID) != nil || validateProviderID(id.IssuerID) != nil || validateProviderID(id.SlotID) != nil {
		return ProviderIdentity{}, false
	}
	return id, true
}

func (c *Client) metadataContainsToken(t TokenMetadata) bool {
	raw, _ := json.Marshal(t)
	return len(c.token) > 0 && strings.Contains(string(raw), string(c.token))
}
