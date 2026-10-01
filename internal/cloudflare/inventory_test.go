package cloudflare_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
)

func metadataToken(id int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("%032x", id), "name": "ordinary legacy label", "status": "active", "value": "CANARY-ignored-token-value", "policies": []any{}}
}

func TestTokenInventoryPagesRetainOnlyMetadataAndDoNotClaimAccountVisibility(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	for i := 1; i <= 65; i++ {
		srv.MetadataTokens = append(srv.MetadataTokens, metadataToken(i))
	}
	got, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID)
	if err != nil || len(got.Tokens) != 65 || !got.PaginationComplete || got.Visibility != "unknown_may_be_creator_only" {
		t.Fatalf("inventory %v error %v", got, err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "CANARY-ignored-token-value") {
		t.Fatal("inventory retained a token value")
	}
	requests := srv.Requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Query, "page=2") || !strings.Contains(requests[0].Query, "per_page=50") || !strings.Contains(requests[0].Query, "include_expired=true") {
		t.Fatalf("requests %#v", requests)
	}
	detail, err := client.TokenDetails(t.Context(), cloudflaretest.AccountID, fmt.Sprintf("%032x", 1))
	if err != nil || detail.ID != got.Tokens[0].ID {
		t.Fatalf("detail %v %v", detail, err)
	}
	encoded, _ = json.Marshal(detail)
	if strings.Contains(string(encoded), "CANARY-ignored-token-value") {
		t.Fatal("detail retained a token value")
	}
}

func TestTokenInventoryPreservesPartialDataAndRejectsRepeatedPages(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	for i := 1; i <= 51; i++ {
		srv.MetadataTokens = append(srv.MetadataTokens, metadataToken(i))
	}
	first := map[string]any{"success": true, "result": srv.MetadataTokens[:50], "result_info": map[string]int{"page": 1, "per_page": 50, "count": 50, "total_count": 51}}
	raw, _ := json.Marshal(first)
	srv.Fail(cloudflaretest.RouteListTokens, cloudflaretest.Failure{Status: 200, RawBody: string(raw), Times: 2})
	got, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID)
	if err == nil || len(got.Tokens) != 50 || got.PaginationComplete {
		t.Fatalf("repeated pages accepted: %v %v", got, err)
	}
}

func TestTokenInventoryPermissionFailureAndReflectedSecretRemainUnknown(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			client, srv, _ := newClient(t)
			srv.Fail(cloudflaretest.RouteListTokens, cloudflaretest.Failure{Status: status, Message: canary})
			got, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID)
			if err == nil || got.PaginationComplete || strings.Contains(err.Error(), canary) {
				t.Fatalf("permission failure: %v %v", got, err)
			}
		})
	}
	t.Run("reflected-name", func(t *testing.T) {
		t.Parallel()
		client, srv, _ := newClient(t)
		token := metadataToken(1)
		token["name"] = canary
		srv.MetadataTokens = []map[string]any{token}
		got, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID)
		if err == nil || len(got.Tokens) != 0 {
			t.Fatal("reflected secret retained as metadata")
		}
	})
}

func TestTokenInventoryEnforcesResponseAndContextBounds(t *testing.T) {
	t.Parallel()
	client, srv, _ := newClient(t)
	srv.Fail(cloudflaretest.RouteListTokens, cloudflaretest.Failure{Status: 200, RawBody: strings.Repeat(" ", 1<<20) + `{"success":true,"result":[]}`})
	if got, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID); err == nil || got.PaginationComplete {
		t.Fatal("oversized answer accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := client.TokenInventory(ctx, cloudflaretest.AccountID); err == nil || got.PaginationComplete {
		t.Fatal("cancelled inventory accepted")
	}
}

func TestCanonicalProviderNameFitsLimitAndRejectsOversizedProposal(t *testing.T) {
	t.Parallel()
	name := "agent-archive r=" + strings.Repeat("a", 32) + " i=" + strings.Repeat("b", 32) + " k=" + strings.Repeat("c", 32)
	id, ok := cloudflare.ParseProviderName(name)
	if !ok || len(name) != 118 || id.RecipientID != strings.Repeat("a", 32) {
		t.Fatal("canonical provider name refused")
	}
	for _, value := range []string{name + "xx", strings.Replace(name, "agent-archive ", "agent-archive abc ", 1), strings.ToUpper(name), strings.Replace(name, " k=", " r=", 1), name[:117]} {
		if _, ok := cloudflare.ParseProviderName(value); ok {
			t.Fatalf("accepted noncanonical name %q", value)
		}
	}
	client, srv, _ := newClient(t)
	srv.MetadataTokens = []map[string]any{{"id": strings.Repeat("a", 32), "name": strings.Replace(name, "agent-archive ", "agent-archive abc ", 1), "status": "active"}}
	if _, err := client.TokenInventory(t.Context(), cloudflaretest.AccountID); err == nil {
		t.Fatal("oversized metadata name accepted")
	}
}

func TestExactBucketPolicyRequiresOnlyVerifiedPermissionAndDestination(t *testing.T) {
	t.Parallel()
	const permission = "aaaa0000000000000000000000000002"
	resource, _ := cloudflare.BucketResource(cloudflaretest.AccountID, cloudflare.BucketRef{Name: "test-bucket", Jurisdiction: "eu"})
	for _, tc := range []struct {
		name      string
		effect    string
		resources any
		want      bool
	}{
		{"exact", "allow", map[string]string{resource: "*"}, true},
		{"other-account", "allow", map[string]string{strings.Replace(resource, cloudflaretest.AccountID, strings.Repeat("b", 32), 1): "*"}, false},
		{"other-jurisdiction", "allow", map[string]string{strings.Replace(resource, "_eu_", "_default_", 1): "*"}, false},
		{"broader", "allow", map[string]string{resource: "*", "com.cloudflare.api.account.*": "*"}, false},
		{"deny", "deny", map[string]string{resource: "*"}, false},
		{"nested", "allow", map[string]any{resource: map[string]string{"*": "*"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, _ := json.Marshal(map[string]any{"id": strings.Repeat("a", 32), "policies": []any{map[string]any{"effect": tc.effect, "resources": tc.resources, "permission_groups": []any{map[string]string{"id": permission}}}}})
			var token cloudflare.TokenMetadata
			if err := json.Unmarshal(raw, &token); err != nil {
				t.Fatal(err)
			}
			if got := cloudflare.ExactBucketPolicy(token, cloudflaretest.AccountID, cloudflare.BucketRef{Name: "test-bucket", Jurisdiction: "eu"}, permission); got != tc.want {
				t.Fatalf("policy %v want %v", got, tc.want)
			}
		})
	}
}
