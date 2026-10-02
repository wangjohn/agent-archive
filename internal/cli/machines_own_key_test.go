package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func ownKeyFixture(t *testing.T) (Env, string, *cloudflaretest.Server, *fakeKeychain, config.Config) {
	t.Helper()
	env, home, cf, kc := dedicatedFixture(t)
	cfg, _, err := config.Load(home)
	must(t, err)
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Shared, AccessKeyID: strings.Repeat("e", 32), SharedWith: strings.Repeat("b", 32)}
	must(t, config.Save(home, cfg))
	env.LookupEnv = func(key string) (string, bool) {
		values := map[string]string{"AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS": "1", "CLOUDFLARE_API_TOKEN": bootstrapCanary}
		value, ok := values[key]
		return value, ok
	}
	return env, home, cf, kc, cfg
}

func TestOwnKeyFailedTransactionRetainsSharedAccessAndRetryReusesStage(t *testing.T) {
	env, home, cf, kc, before := ownKeyFixture(t)
	failed := false
	fakeSched(env).beforeLoad = func(scheduler.Ref) error {
		if !failed {
			failed = true
			return errors.New("synthetic transaction load failure")
		}
		return nil
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 1 {
		t.Fatal("failed transaction committed")
	}
	after, _, err := config.Load(home)
	must(t, err)
	if after.Storage.R2CredentialRef != before.Storage.R2CredentialRef || after.MachineAssignment.Kind != config.MachineAssignmentR2Shared {
		t.Fatal("rollback lost shared binding")
	}
	if _, err = kc.Load(t.Context(), before.Storage.R2CredentialRef); err != nil {
		t.Fatal("rollback removed shared secret")
	}
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	output.Reset()
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
		t.Fatalf("retry duplicated stage: %d %s", code, output.String())
	}
}

func TestOwnKeyCleanupPendingRecoveryRechecksRetainedBindings(t *testing.T) {
	env, home, cf, _, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	slot.State = issuance.CleanupPending
	slot.CleanupReason = "uncommitted-own-transaction"
	must(t, issuance.Save(home, slot))
	previous := cfg.Storage
	previous.Bucket = "retained-destination"
	previous.R2CredentialRef = slot.SecretRef
	cfg.PreviousDestinations = []credentials.Config{previous}
	must(t, config.Save(home, cfg))
	for _, args := range [][]string{{"machines", "own-key", "--cancel", "--yes"}, {"machines", "own-key", "--yes"}} {
		var output bytes.Buffer
		if code := Run(args, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, slot.ProviderID) {
			t.Fatalf("recovery bypassed retained binding proof: %v %d %s", args, code, output.String())
		}
	}
}

func TestOwnKeyLostCreationIdentityCannotDeleteAliasedActiveCredential(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	key, err := kc.Load(t.Context(), slot.SecretRef)
	must(t, err)
	must(t, kc.Save(t.Context(), cfg.Storage.R2CredentialRef, key))
	// Model a crash after provider creation but before its ID was journaled.
	slot.State = issuance.CreationIntent
	slot.ProviderID = ""
	must(t, local.Write(filepath.Join(home, "issued", "slot-"+slot.SlotID+".json"), slot))
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, key.AccessKeyID) {
		t.Fatalf("orphan lookup bypassed active credential proof: %d %s", code, output.String())
	}
}

func TestAddReconciliationLeavesInterruptedOwnKeyStageForItsOwningOperation(t *testing.T) {
	env, home, cf, _, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	// Crash after credential storage, before the OwnIntent ledger save.
	slot.State = issuance.SecretIntent
	must(t, local.Write(filepath.Join(home, "issued", "slot-"+slot.SlotID+".json"), slot))
	must(t, issuer.reconcile())
	if cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, slot.ProviderID) {
		t.Fatal("add cleanup consumed another operation's own-key stage")
	}
}

func TestOwnKeyCancellationRetainsPreviousDestinationExactAndAliasedKeys(t *testing.T) {
	for _, binding := range []string{"exact", "alias", "unknown"} {
		t.Run(binding, func(t *testing.T) {
			env, home, cf, kc, cfg := ownKeyFixture(t)
			issuer := fixtureIssuer(t, env, home)
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
			slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
			must(t, err)
			previous := cfg.Storage
			previous.Bucket = "retained-destination"
			previous.R2CredentialRef = slot.SecretRef
			if binding != "exact" {
				previous.R2CredentialRef = "retained-alias"
				if binding == "alias" {
					key, loadErr := kc.Load(t.Context(), slot.SecretRef)
					must(t, loadErr)
					must(t, kc.Save(t.Context(), previous.R2CredentialRef, key))
				}
			}
			cfg.PreviousDestinations = []credentials.Config{previous}
			must(t, config.Save(home, cfg))
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, slot.ProviderID) {
				t.Fatalf("retained %s binding deleted: %d %s", binding, code, output.String())
			}
		})
	}
}

func TestOwnKeyCommitsWithExistingMachineIdentityAndThenRemovesSharedLocalSecret(t *testing.T) {
	env, home, cf, kc, before := ownKeyFixture(t)
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("%d %s", code, output.String())
	}
	after, _, err := config.Load(home)
	must(t, err)
	if after.MachineID != before.MachineID || after.MachineAssignment.Kind != config.MachineAssignmentR2Own || after.MachineAssignment.RecipientID != before.MachineID || after.Storage.R2CredentialRef == before.Storage.R2CredentialRef || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatal("identity changed or shared provider key deleted")
	}
	if _, err = kc.Load(context.Background(), before.Storage.R2CredentialRef); !errors.Is(err, credentials.ErrMissingCredential) {
		t.Fatal("old local shared secret retained")
	}
	raw, err := os.ReadFile(filepath.Join(home, ownKeyFile))
	must(t, err)
	if strings.Contains(string(raw), "object-canary") || strings.Contains(string(raw), bootstrapCanary) {
		t.Fatal("secret checkpoint")
	}
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	output.Reset()
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
		t.Fatal("committed retry minted duplicate")
	}
}

func TestOwnKeyRetainsSharedSecretNeededByAnotherLocalDestinationAndPublicationIsIndependent(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	prior := cfg.Storage
	prior.Bucket = "previous"
	cfg.PreviousDestinations = []credentials.Config{prior}
	must(t, config.Save(home, cfg))
	env.OpenStore = func(candidate config.Config) (storage.ObjectStore, error) {
		if candidate.MachineID == "" {
			return storagetest.NewMemoryStore(), nil
		}
		return nil, errors.New("synthetic final publication failure")
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || !strings.Contains(output.String(), "Old shared access remains") || !strings.Contains(output.String(), "registration pending") {
		t.Fatalf("%d %s", code, output.String())
	}
	if _, err := kc.Load(t.Context(), cfg.Storage.R2CredentialRef); err != nil || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatal("retained shared access deleted")
	}
}

func TestOwnKeyReportsSharedAccessRetainedThroughAnotherDestinationAlias(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	key, err := kc.Load(t.Context(), cfg.Storage.R2CredentialRef)
	must(t, err)
	must(t, kc.Save(t.Context(), "shared-alias", key))
	previous := cfg.Storage
	previous.Bucket = "retained-destination"
	previous.R2CredentialRef = "shared-alias"
	cfg.PreviousDestinations = []credentials.Config{previous}
	must(t, config.Save(home, cfg))
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || !strings.Contains(output.String(), "Old shared access remains") || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatalf("retained alias not disclosed: %d %s", code, output.String())
	}
	if _, err = kc.Load(t.Context(), "shared-alias"); err != nil {
		t.Fatal("other destination credential removed")
	}
}

func TestOwnKeyStagedRetryUsesCheckpointSlotWithoutMintingAgain(t *testing.T) {
	env, home, cf, _, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	var saved ownKeyCheckpoint
	must(t, local.Read(filepath.Join(home, ownKeyFile), &saved))
	if saved.SlotID != slot.SlotID {
		t.Fatal("exact slot not checkpointed")
	}
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	recovered, err := ownKeySlot(home, &saved, issuer, issuer.api, env)
	must(t, err)
	after, _, err := config.Load(home)
	must(t, err)
	if recovered.SlotID != slot.SlotID || cf.Calls(cloudflaretest.RouteCreateToken) != creates || after.Storage.R2CredentialRef != cfg.Storage.R2CredentialRef {
		t.Fatal("retry changed active config or minted again before commit")
	}
}

func TestOwnKeyCancellationOnlyDeletesUncommittedIntent(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	if slot.State != issuance.OwnIntent {
		t.Fatal("stage became committed ownership prematurely")
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("%d %s", code, output.String())
	}
	after, _, err := config.Load(home)
	must(t, err)
	if after.Storage.R2CredentialRef != cfg.Storage.R2CredentialRef || cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
		t.Fatal("cancel changed shared configuration or missed dedicated delete")
	}
	if _, err = kc.Load(t.Context(), cfg.Storage.R2CredentialRef); err != nil {
		t.Fatal("shared secret removed")
	}
	if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
		t.Fatal("cancel checkpoint retained")
	}
}

func TestOwnKeyCommittedCancellationRefusesBeforeProvider(t *testing.T) {
	env, home, cf, _, _ := ownKeyFixture(t)
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
		t.Fatalf("%d %s", code, output.String())
	}
	slots, err := issuance.List(home)
	must(t, err)
	if len(slots) != 1 || slots[0].State != issuance.Own {
		t.Fatal("committed ownership not promoted")
	}
	before := cf.Calls(cloudflaretest.RouteDeleteToken)
	output.Reset()
	if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != before {
		t.Fatal("committed cancellation deleted active key")
	}
}

func TestDedicatedOwnIntentCleanupPreservesCommittedOrUnknownBindings(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	s, _, err := i.createWithIntent(issuance.Fresh, i.cfg.MachineID, func(issuance.Slot) error { return nil })
	must(t, err)
	cfg := i.cfg
	cfg.Storage.R2CredentialRef = s.SecretRef
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: s.ProviderID, RecipientID: s.RecipientID, IssuerID: s.IssuerID, SlotID: s.SlotID}
	must(t, config.Save(home, cfg))
	if i.cleanupUncommittedOwnIntent(&s) == nil || len(cf.Live()) != 1 {
		t.Fatal("committed key deleted")
	}
	s.State = issuance.Own
	must(t, issuance.Save(home, s))
	i.cleanup(&s)
	if len(cf.Live()) != 1 {
		t.Fatal("committed own key cleaned automatically")
	}
	// A different reference may still load the same active provider key.
	second, key, err := i.createWithIntent(issuance.Fresh, i.cfg.MachineID, func(issuance.Slot) error { return nil })
	must(t, err)
	cfg = i.cfg
	s = second
	must(t, kc.Save(context.Background(), "main", key))
	must(t, config.Save(home, cfg))
	if i.cleanupUncommittedOwnIntent(&s) == nil || len(cf.Live()) != 2 {
		t.Fatal("aliased active key deleted")
	}
	must(t, kc.Delete(context.Background(), "main"))
	if i.cleanupUncommittedOwnIntent(&s) == nil || len(cf.Live()) != 2 {
		t.Fatal("unknown active binding deleted")
	}
}
