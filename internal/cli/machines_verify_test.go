package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestMachinesVerifyConsumerPreservesUnknownVisibilityAndLocalTrust(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Now().UTC()
	env := testEnv(t, home, now)
	server := cloudflaretest.New(t, "MANAGEMENT-CANARY")
	key := strings.Repeat("d", 32)
	recipient := strings.Repeat("a", 32)
	issuer := strings.Repeat("b", 32)
	slot := strings.Repeat("c", 32)
	cfg := config.Config{MachineID: recipient, MachineName: "laptop", Storage: credentials.Config{Provider: credentials.ProviderR2, R2AccountID: cloudflaretest.AccountID, Bucket: "test-bucket"}, CloudflareTokenCommand: []string{"must-not-execute"}}
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: "r2_own", AccessKeyID: key, RecipientID: recipient, IssuerID: issuer, SlotID: slot}
	must(t, config.Save(home, cfg))
	store := storagetest.NewMemoryStore()
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", now)
	must(t, err)
	must(t, machines.Publish(context.Background(), store, record))
	other := record
	other.MachineID = strings.Repeat("e", 32)
	other.Name = "forged-laptop"
	must(t, machines.Publish(context.Background(), store, other))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	calls, removed := 0, false
	env.Cloudflare = func(token string) cloudflare.API {
		calls++
		if token != "MANAGEMENT-CANARY" {
			t.Fatal("incorrect source")
		}
		return cloudflare.New(token, cloudflare.Options{BaseURL: server.URL + "/client/v4"})
	}
	env.LookupEnv = func(k string) (string, bool) {
		values := map[string]string{"AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY": "1", "CLOUDFLARE_API_TOKEN": "MANAGEMENT-CANARY"}
		value, ok := values[k]
		return value, ok
	}
	env.UnsetEnv = func(string) error { removed = true; return nil }
	env.RunTokenCommand = func(context.Context, []string, []string) (string, error) {
		t.Fatal("ordinary or unattended invocation ran password manager")
		return "", nil
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "--json"}, nil, &output, &output, env); code != 0 || calls != 0 || removed {
		t.Fatalf("ordinary listing consumed token: %d %s", code, output.String())
	}
	// The provider evidence confirms issuance and scope, never the other machine's ownership.
	permission := server.Groups[1].ID
	resource, err := cloudflare.BucketResource(cloudflaretest.AccountID, cloudflare.BucketRef{Name: "test-bucket"})
	must(t, err)
	server.MetadataTokens = []map[string]any{{"id": key, "name": "agent-archive r=" + recipient + " i=" + issuer + " k=" + slot, "status": "active", "value": "IGNORED-CANARY", "policies": []any{map[string]any{"effect": "allow", "permission_groups": []any{map[string]string{"id": permission}}, "resources": map[string]string{resource: "*"}}}}}
	output.Reset()
	if code := Run([]string{"machines", "--verify", "--yes", "--json"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("verify %d %s", code, output.String())
	}
	var got verifiedMachinesResult
	must(t, json.Unmarshal(output.Bytes(), &got))
	if !removed || calls != 1 || !got.Verification.PaginationComplete || got.Verification.AccountInventoryComplete || got.Verification.Visibility != "unknown_may_be_creator_only" || len(got.Verification.Observations) != 2 {
		t.Fatalf("false completeness %#v", got.Verification)
	}
	for _, observation := range got.Verification.Observations {
		if observation.MachineID == recipient && observation.Binding != "local_committed_binding" || observation.MachineID != recipient && observation.Binding != "untrusted_bucket_claim" {
			t.Fatalf("forged trust %#v", observation)
		}
	}
	if strings.Contains(output.String(), "CANARY") {
		t.Fatal("provider token or ignored value leaked")
	}
	must(t, filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if bytes.Contains(raw, []byte("CANARY")) {
			t.Errorf("management token persisted in %s", path)
		}
		return nil
	}))
	server.MetadataTokens = nil
	output.Reset()
	if code := Run([]string{"machines", "--verify", "--yes", "--json"}, nil, &output, &output, env); code != 1 {
		t.Fatal("missing or not-visible was treated as verified")
	}
	must(t, json.Unmarshal(output.Bytes(), &got))
	if got.ProviderVerified || !got.Verification.Partial || got.Verification.Observations[0].State != observationMissing {
		t.Fatalf("missing evidence %#v", got)
	}
}

func TestProviderDestinationRefusesArbitraryEndpoints(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"https://evil.example", cloudflare.Endpoint(cloudflaretest.AccountID, "") + "/path", "http://" + cloudflaretest.AccountID + ".r2.cloudflarestorage.com", "https://" + cloudflaretest.AccountID + ".r2.cloudflarestorage.com.evil.example"} {
		if _, _, err := providerDestination(credentials.Config{Provider: credentials.ProviderR2, R2AccountID: cloudflaretest.AccountID, Bucket: "test-bucket", R2Endpoint: endpoint}); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	account, bucket, err := providerDestination(credentials.Config{Provider: credentials.ProviderR2, Bucket: "test-bucket", R2Endpoint: cloudflare.Endpoint(cloudflaretest.AccountID, "eu")})
	if err != nil || account != cloudflaretest.AccountID || bucket.Jurisdiction != "eu" {
		t.Fatal("valid jurisdiction rejected")
	}
	if _, _, err := providerDestination(credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket"}); err == nil {
		t.Fatal("S3 management accepted")
	}
}

func TestProviderBindingStatesDoNotInferIdentityFromNameAlone(t *testing.T) {
	t.Parallel()
	now := time.Now()
	recipient, issuer, slot := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	binding := machines.CredentialBinding{Kind: "r2_own", RecipientID: recipient, IssuerID: issuer, SlotID: slot}
	bucket := cloudflare.BucketRef{Name: "test-bucket"}
	resource, err := cloudflare.BucketResource(cloudflaretest.AccountID, bucket)
	must(t, err)
	token := cloudflare.TokenMetadata{Name: "agent-archive r=" + recipient + " i=" + issuer + " k=" + slot, Status: cloudflare.TokenStatusActive, Policies: []cloudflare.MetadataPolicy{{Effect: "allow", PermissionGroups: []cloudflare.MetadataPermissionGroup{{ID: "verified-group"}}, Resources: map[string]string{resource: "*"}}}}
	state, complete := providerBindingState(token, true, binding, cloudflaretest.AccountID, bucket, "verified-group", now)
	if state != observationMatches || !complete {
		t.Fatal("exact issuance evidence rejected")
	}
	state, complete = providerBindingState(token, true, binding, cloudflaretest.AccountID, cloudflare.BucketRef{Name: "other"}, "verified-group", now)
	if state != observationScope || complete {
		t.Fatal("name bypassed destination policy")
	}
	binding.RecipientID = issuer
	state, complete = providerBindingState(token, true, binding, cloudflaretest.AccountID, bucket, "verified-group", now)
	if state != observationIssuance || complete {
		t.Fatal("forged recipient passed")
	}
	token.Status = cloudflare.TokenStatusDisabled
	state, complete = providerBindingState(token, true, binding, cloudflaretest.AccountID, bucket, "verified-group", now)
	if state != observationInactive || complete {
		t.Fatal("disabled key active")
	}
	token.Status = cloudflare.TokenStatusActive
	token.ExpiresOn = now.Add(-time.Second).Format(time.RFC3339)
	if providerTokenActive(token, now) {
		t.Fatal("expired key active")
	}
	token.ExpiresOn = ""
	token.NotBefore = now.Add(time.Hour).Format(time.RFC3339)
	if providerTokenActive(token, now) {
		t.Fatal("future key active")
	}
}
