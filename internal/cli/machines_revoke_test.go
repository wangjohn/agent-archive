package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/revocation"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func revocationFixture(t *testing.T) (Env, string, *cloudflaretest.Server, config.Config, *storagetest.MemoryStore) {
	t.Helper()
	env, home, cf, _ := dedicatedFixture(t)
	issuer := fixtureIssuer(t, env, home)
	slot, _, err := issuer.create(issuance.Fresh)
	must(t, err)
	slot.State = issuance.Own
	// Construct a synthetic preexisting committed own slot; creation transitions are separately tested by issuance.
	must(t, local.Write(filepath.Join(home, "issued", "slot-"+slot.SlotID+".json"), slot))
	cfg, _, err := config.Load(home)
	must(t, err)
	cfg.Storage.R2CredentialRef = slot.SecretRef
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID}
	must(t, config.Save(home, cfg))
	store := storagetest.NewMemoryStore()
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", env.now())
	must(t, err)
	must(t, machines.Publish(t.Context(), store, record))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	env.LookupEnv = func(key string) (string, bool) {
		values := map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary}
		value, ok := values[key]
		return value, ok
	}
	return env, home, cf, cfg, store
}

func TestRevokeRefusesPendingSetupRecoveryBeforeProvider(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	must(t, os.WriteFile(setupjournal.JournalPath(home), []byte("{}"), 0600))
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !strings.Contains(output.String(), "recovery pending") {
		t.Fatalf("pending transaction authorized revocation: %d %s", code, output.String())
	}
}

func TestRevocationRetryRefusesJournalScopeDifferentFromCommittedDestination(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	other := cfg
	other.Storage.Bucket = "different-bucket"
	issuer, err := newKeyIssuer(home, other, env, newPrompter(nil, &bytes.Buffer{}), env.cloudflareAPI(bootstrapCanary))
	must(t, err)
	defer issuer.api.Discard()
	slot, _, err := issuer.create(issuance.Fresh)
	must(t, err)
	j, err := prepareRevocation(home, cfg, revokeSelector{MachineID: cfg.MachineID}, "", env)
	must(t, err)
	j.Bucket = "different-bucket"
	j.PermissionID = slot.PermissionID
	j.Keys = []revocation.Key{{ProviderID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID, Outcome: revocation.Pending}}
	must(t, revocation.Save(home, j))
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--operation-id", j.OperationID, "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !strings.Contains(output.String(), "provider scope") {
		t.Fatalf("journal destination mismatch accepted: %d %s", code, output.String())
	}
}

func TestRevokeHoldsIssuanceAndSetupLocksAndRetiresConfirmedSpare(t *testing.T) {
	env, home, _, cfg, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	spare, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	open := env.OpenStore
	env.OpenStore = func(candidate config.Config) (storage.ObjectStore, error) {
		for _, name := range []string{"issued.lock", "setup.lock"} {
			if unlock, lockErr := local.NamedLock(home, name); lockErr == nil {
				unlock()
				t.Errorf("revocation released %s before provider execution", name)
			}
		}
		return open(candidate)
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("%d %s", code, output.String())
	}
	slots, err := issuance.List(home)
	must(t, err)
	for _, slot := range slots {
		if slot.SlotID == spare.SlotID && slot.State != issuance.Deleted {
			t.Fatalf("confirmed revoked spare remains eligible: %s", slot.State)
		}
	}
}

func TestRevokeSelfUsesImmutableBindingKeepsActiveLastAndNeverRefills(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	spare, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	delivered, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	delivered.State = issuance.Reserved
	delivered.PairingID = strings.Repeat("a", 32)
	delivered.Label = "recipient"
	delivered.ExpiresAt = env.now().Add(1000000000)
	must(t, issuance.Save(home, delivered))
	delivered.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, delivered))
	delivered.State = issuance.Delivered
	must(t, issuance.Save(home, delivered))
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes", "--json"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("%d %s", code, output.String())
	}
	// JSON is written to stdout; the verified selection is separately printed on stderr.
	start := strings.Index(output.String(), "{\"version\"")
	if start < 0 {
		t.Fatal(output.String())
	}
	var journal revocation.Journal
	must(t, json.Unmarshal(output.Bytes()[start:], &journal))
	if !journal.Complete() || len(journal.Keys) != 2 || journal.Keys[1].ProviderID != cfg.MachineAssignment.AccessKeyID || !providerKeyLive(cf, delivered.ProviderID) || providerKeyLive(cf, spare.ProviderID) || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
		t.Fatalf("wrong set %#v", journal)
	}
	localJournal, err := revocation.Load(home, journal.OperationID)
	must(t, err)
	if !localJournal.Complete() {
		t.Fatal("provider outcomes not durable")
	}
	output.Reset()
	if code := Run([]string{"machines", "revoke", "--operation-id", journal.OperationID, "--yes", "--json"}, nil, &output, &output, env); code != 0 {
		t.Fatal("confirmed retry regressed")
	}
}

func TestRevokeRefusesForgedMappingAndUnverified404(t *testing.T) {
	env, home, cf, cfg, store := revocationFixture(t)
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", env.now())
	must(t, err)
	record.MachineID = strings.Repeat("f", 32)
	record.Name = "forged"
	must(t, machines.Publish(t.Context(), store, record))
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "forged", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatal("bucket claim authorized deletion")
	}
	cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: 404, Message: "not found"})
	output.Reset()
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &output, &output, env); code != 1 || strings.Contains(output.String(), "Access removed for") {
		t.Fatalf("404 counted confirmed: %s", output.String())
	}
	entries, err := os.ReadDir(filepath.Join(home, "revocations"))
	must(t, err)
	if len(entries) != 1 {
		t.Fatal("operation not tracked")
	}
}

func TestRevokeNoTokenOnlyRequestsAndProviderSuccessSurvivesPublicationFailure(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	env.LookupEnv = func(string) (string, bool) { return "", false }
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !strings.Contains(output.String(), "access not removed") {
		t.Fatal("request claimed removal")
	}
	env.LookupEnv = func(key string) (string, bool) {
		values := map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary}
		value, ok := values[key]
		return value, ok
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return nil, errors.New("synthetic publication failure")
	}
	output.Reset()
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &output, &output, env); code != 0 || !strings.Contains(output.String(), "publication pending") {
		t.Fatalf("%d %s", code, output.String())
	}
	entries, err := os.ReadDir(filepath.Join(home, "revocations"))
	must(t, err)
	confirmed := false
	for _, entry := range entries {
		var journal revocation.Journal
		must(t, local.Read(filepath.Join(home, "revocations", entry.Name()), &journal))
		confirmed = confirmed || journal.Complete()
	}
	if !confirmed {
		t.Fatal("final publication failure discarded provider result")
	}
}

func providerKeyLive(cf *cloudflaretest.Server, id string) bool {
	for _, key := range cf.Live() {
		if key.ID == id {
			return true
		}
	}
	return false
}

func TestIncludeIssuedCoversDeliveredRecipientsDespiteOrdinaryIssuerExclusion(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	delivered, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	delivered.State = issuance.Reserved
	delivered.PairingID = strings.Repeat("b", 32)
	delivered.Label = "delivered"
	delivered.ExpiresAt = env.now().Add(time.Hour)
	must(t, issuance.Save(home, delivered))
	delivered.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, delivered))
	delivered.State = issuance.Delivered
	must(t, issuance.Save(home, delivered))
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--include-issued", "--yes"}, nil, &output, &output, env); code != 0 || providerKeyLive(cf, delivered.ProviderID) {
		t.Fatalf("delivered issuer descendant omitted: %d %s", code, output.String())
	}
}

func TestRecipientLedgerSelectionIgnoresForgedLabelAndProviderScopeMismatch(t *testing.T) {
	env, home, cf, _, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	spare, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--recipient-id", spare.RecipientID, "--yes"}, nil, &output, &output, env); code != 0 || providerKeyLive(cf, spare.ProviderID) {
		t.Fatalf("healthy issuer recipient refused: %d %s", code, output.String())
	}
	other, _, err := issuer.create(issuance.Fresh)
	must(t, err)
	resource, err := cloudflare.BucketResource(cloudflaretest.AccountID, cloudflare.BucketRef{Name: "other"})
	must(t, err)
	cf.MetadataTokens = []map[string]any{{"id": other.ProviderID, "name": other.ProviderName, "status": "active", "policies": []any{map[string]any{"effect": "allow", "permission_groups": []any{map[string]string{"id": other.PermissionID}}, "resources": map[string]string{resource: "*"}}}}}
	output.Reset()
	if code := Run([]string{"machines", "revoke", "--recipient-id", other.RecipientID, "--yes"}, nil, &output, &output, env); code != 1 || !providerKeyLive(cf, other.ProviderID) {
		t.Fatal("wrong-destination lineage authorized deletion")
	}
}

func TestRevokeCancelledBudgetPreservesPendingExactSelection(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	selector := revokeSelector{MachineID: cfg.MachineID}
	j, err := prepareRevocation(home, cfg, selector, "", env)
	must(t, err)
	api := env.cloudflareAPI(bootstrapCanary)
	defer api.Discard()
	reader := api.(cloudflare.InventoryAPI)
	j, err = selectRevocation(t.Context(), home, cfg, selector, j, api, reader, env)
	must(t, err)
	must(t, revocation.Save(home, j))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = executeRevocation(ctx, home, &j, cfg, api, reader, env); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled budget did not report interruption", err)
	}
	if j.Complete() || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || j.Keys[0].Outcome != revocation.Pending {
		t.Fatal("cancelled budget claimed removal")
	}
	saved, err := revocation.Load(home, j.OperationID)
	must(t, err)
	if saved.Keys[0].ProviderID != j.Keys[0].ProviderID || saved.Keys[0].Outcome != revocation.Pending {
		t.Fatal("budget lost exact pending selection")
	}
}

func TestRevocationProgressPublicationDoesNotCancelProviderLoop(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	_, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	selector := revokeSelector{MachineID: cfg.MachineID}
	j, err := prepareRevocation(home, cfg, selector, "", env)
	must(t, err)
	reader := issuer.api.(cloudflare.InventoryAPI)
	j, err = selectRevocation(t.Context(), home, cfg, selector, j, issuer.api, reader, env)
	must(t, err)
	must(t, revocation.Save(home, j))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		// Deterministically model publication exhausting a provider deadline.
		cancel()
		return nil, errors.New("synthetic slow publication failure")
	}
	if err = executeRevocation(ctx, home, &j, cfg, issuer.api, reader, env); err != nil || !j.Complete() || cf.Calls(cloudflaretest.RouteDeleteToken) != len(j.Keys) || !j.PublicationPending {
		t.Fatalf("publication interrupted provider results: %v %#v", err, j)
	}
}

func TestUnknownRevocationWithholdsSpareAndConfirmedRetryFinishesLedger(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	issuer := fixtureIssuer(t, env, home)
	spare, _, err := issuer.create(issuance.Precreated)
	must(t, err)
	cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: 404, Message: "not found"})
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--recipient-id", spare.RecipientID, "--yes"}, nil, &output, &output, env); code != 1 {
		t.Fatal("unverified absence claimed success")
	}
	slots, err := issuance.List(home)
	must(t, err)
	for _, slot := range slots {
		if slot.SlotID == spare.SlotID && slot.State != issuance.CleanupPending {
			t.Fatalf("uncertain revoked key can be delivered: %s", slot.State)
		}
	}
	// Model a crash after successful DELETE was journaled but before ledger retirement.
	j, err := prepareRevocation(home, cfg, revokeSelector{RecipientID: spare.RecipientID}, "", env)
	must(t, err)
	j.PermissionID = spare.PermissionID
	j.Keys = []revocation.Key{{ProviderID: spare.ProviderID, RecipientID: spare.RecipientID, IssuerID: spare.IssuerID, SlotID: spare.SlotID, Outcome: revocation.Confirmed}}
	must(t, revocation.Save(home, j))
	deletes := cf.Calls(cloudflaretest.RouteDeleteToken)
	output.Reset()
	if code := Run([]string{"machines", "revoke", "--operation-id", j.OperationID, "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != deletes {
		t.Fatalf("confirmed recovery repeated provider operation: %d %s", code, output.String())
	}
	slots, err = issuance.List(home)
	must(t, err)
	for _, slot := range slots {
		if slot.SlotID == spare.SlotID && slot.State != issuance.Deleted {
			t.Fatal("confirmed retry did not retire ledger")
		}
	}
}

// A bucket writer can replace this machine's label without changing its key.
func TestNamedRevocationRefusesForgedSelfLabel(t *testing.T) {
	env, _, cf, cfg, store := revocationFixture(t)
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", env.now())
	must(t, err)
	record.Name = "victim"
	must(t, machines.Publish(t.Context(), store, record))
	var output bytes.Buffer
	if code := Run([]string{"machines", "revoke", "victim", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, cfg.MachineAssignment.AccessKeyID) {
		t.Fatalf("forged label authorized self deletion: %d %s", code, output.String())
	}
}

func TestRevocationPublicationKeepsRegistryCommandsUsable(t *testing.T) {
	env, home, _, cfg, store := revocationFixture(t)
	cfg.MachineName = "local-source"
	must(t, config.Save(home, cfg))
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", env.now())
	must(t, err)
	must(t, machines.Publish(t.Context(), store, record))
	j, err := prepareRevocation(home, cfg, revokeSelector{Name: "local-source"}, "", env)
	must(t, err)
	j.RequestOnly = true
	must(t, putRevocation(t.Context(), store, j))
	lookup := env.LookupEnv
	env.LookupEnv = func(key string) (string, bool) {
		return lookup(key)
	}
	for _, args := range [][]string{{"machines", "--json"}, {"machines", "--verify", "--yes", "--json"}, {"machines", "rename", "renamed-source"}, {"machines", "revoke", "renamed-source", "--yes"}} {
		var output bytes.Buffer
		if code := Run(args, nil, &output, &output, env); code != 0 {
			t.Fatalf("operation poisoned registry command %v: %d %s", args, code, output.String())
		}
	}
}

func TestRequestOnlyRevocationPersistsUnverifiedSelector(t *testing.T) {
	schemaBytes, err := os.ReadFile(filepath.Join("..", "..", "schemas", "revocation.schema.json"))
	must(t, err)
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	must(t, err)
	schemaID := schemaDoc.(map[string]any)["$id"].(string)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	must(t, compiler.AddResource(schemaID, schemaDoc))
	schema, err := compiler.Compile(schemaID)
	must(t, err)
	for _, provider := range []string{"r2", "s3"} {
		for _, kind := range []string{"name", "machine_id", "recipient_id", "pairing_id"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				env, home, cf, cfg, store := revocationFixture(t)
				if provider == "s3" {
					cfg.Storage = credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic", AWSProfile: "synthetic"}
					cfg.MachineAssignment = nil
					must(t, config.Save(home, cfg))
				}
				env.LookupEnv = func(string) (string, bool) { return "", false }
				value := strings.Repeat("b", 32)
				args := []string{"machines", "revoke", "--yes"}
				if kind == "name" {
					value = "requested-target"
					args = append(args, value)
				} else {
					args = append(args, "--"+strings.ReplaceAll(kind, "_", "-"), value)
				}
				var output bytes.Buffer
				if code := Run(args, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
					t.Fatalf("request deleted access: %d %s", code, output.String())
				}
				entries, err := os.ReadDir(filepath.Join(home, "revocations"))
				must(t, err)
				if len(entries) != 1 {
					t.Fatal("request journal missing")
				}
				raw, err := os.ReadFile(filepath.Join(home, "revocations", entries[0].Name()))
				must(t, err)
				var saved map[string]any
				must(t, json.Unmarshal(raw, &saved))
				requested, ok := saved["requested_selector"].(map[string]any)
				if !ok || requested["kind"] != kind || requested["value"] != value || saved["request_only"] != true || len(saved["keys"].([]any)) != 0 || saved["target_id"] != nil {
					t.Fatalf("unverified request lost or promoted: %s", raw)
				}
				published, err := store.Get(t.Context(), "machines/revocations/"+saved["operation_id"].(string)+".json")
				must(t, err)
				var bucket map[string]any
				must(t, json.Unmarshal(published, &bucket))
				schemaValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(published))
				must(t, err)
				must(t, schema.Validate(schemaValue))

				if !reflect.DeepEqual(bucket["requested_selector"], saved["requested_selector"]) || strings.Contains(string(raw)+string(published), bootstrapCanary) || strings.Contains(string(raw)+string(published), "object-canary") {
					t.Fatal("published request lost selector or exposed credential")
				}
				// Bucket hints never replace the original request with a deletion selection.
				operation := saved["operation_id"].(string)
				output.Reset()
				if code := Run([]string{"machines", "revoke", "--operation-id", operation, "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
					t.Fatal("request-only retry acquired deletion authority")
				}
			})
		}
	}
}
