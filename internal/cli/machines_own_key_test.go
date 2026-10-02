package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
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
	if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
		t.Fatal("completed cleanup checkpoint retained", err)
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

func TestCompletedOwnKeyAllowsLaterSharedMigration(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(strconv.FormatBool(different), func(t *testing.T) {
			env, home, cf, kc, initial := ownKeyFixture(t)
			shared, err := kc.Load(t.Context(), initial.Storage.R2CredentialRef)
			must(t, err)
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
				t.Fatalf("initial migration: %d %s", code, output.String())
			}
			owned, _, err := config.Load(home)
			must(t, err)
			priorKey := owned.MachineAssignment.AccessKeyID
			next := owned
			if different {
				next.Storage.Bucket = "other-destination"
			}
			next.Storage.R2CredentialRef = "repaired-shared"
			must(t, kc.Save(t.Context(), next.Storage.R2CredentialRef, shared))
			next.MachineAssignment = &config.MachineAssignment{DestinationID: next.DestinationID(), Kind: config.MachineAssignmentR2Shared, AccessKeyID: shared.AccessKeyID, SharedWith: strings.Repeat("b", 32)}
			userHome, err := env.userHomeDir()
			must(t, err)
			executable, err := env.executable()
			must(t, err)
			must(t, applySetup(home, userHome, executable, owned, &next, nil, env))
			creates := cf.Calls(cloudflaretest.RouteCreateToken)
			output.Reset()
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates+1 {
				t.Fatalf("completed checkpoint blocked later migration: %d %s", code, output.String())
			}
			current, _, err := config.Load(home)
			must(t, err)
			if current.MachineAssignment.AccessKeyID == priorKey || !providerKeyLive(cf, priorKey) || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
				t.Fatal("later migration consumed prior dedicated key")
			}
			slots, err := issuance.List(home)
			must(t, err)
			for _, slot := range slots {
				if slot.ProviderID == priorKey && slot.State != issuance.Own {
					t.Fatal("prior dedicated lifecycle changed")
				}
			}
			creates = cf.Calls(cloudflaretest.RouteCreateToken)
			output.Reset()
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
				t.Fatal("completed retry minted again")
			}
		})
	}
}

func TestOwnKeyDisclosesRetiredSharedAlias(t *testing.T) {
	type bindingKind string
	const (
		alias      bindingKind = "alias"
		unreadable bindingKind = "unreadable"
		unrelated  bindingKind = "unrelated"
	)
	for _, binding := range []bindingKind{alias, unreadable, unrelated} {
		t.Run(string(binding), func(t *testing.T) {
			env, home, cf, kc, cfg := ownKeyFixture(t)
			key, err := kc.Load(t.Context(), cfg.Storage.R2CredentialRef)
			must(t, err)
			retired := cfg.Storage.R2CredentialRef
			must(t, kc.Save(t.Context(), "current-shared", key))
			switch binding {
			case alias:
				// Retain the same shared credential under its previous reference.
			case unreadable:
				must(t, kc.Delete(t.Context(), retired))
			case unrelated:
				must(t, kc.Save(t.Context(), retired, credentials.R2Credentials{AccessKeyID: strings.Repeat("f", 32), SecretAccessKey: "unrelated-canary"}))
			}
			// Same-destination setup creates a new reference and retires the old one.
			next := cfg
			next.Storage.R2CredentialRef = "current-shared"
			userHome, err := env.userHomeDir()
			must(t, err)
			executable, err := env.executable()
			must(t, err)
			must(t, applySetup(home, userHome, executable, cfg, &next, nil, env))
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
				t.Fatalf("migration: %d %s", code, output.String())
			}
			if binding != unrelated && (!strings.Contains(output.String(), "Old shared access remains") || strings.Contains(output.String(), "Old shared local secret removed")) {
				t.Fatalf("retired shared alias falsely removed: %s", output.String())
			}
			if binding != unreadable {
				if _, err = kc.Load(t.Context(), retired); err != nil {
					t.Fatal("retired alias or unrelated credential deleted")
				}
			}
		})
	}
}

func TestOwnKeyRetiresOnlyCompletedCheckpointWithCommittedLedgerProof(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(strconv.FormatBool(pending), func(t *testing.T) {
			env, home, cf, _, cfg := ownKeyFixture(t)
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
				t.Fatalf("migration: %d %s", code, output.String())
			}
			slots, err := issuance.List(home)
			must(t, err)
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef, SlotID: slots[0].SlotID, Committed: true, CleanupPending: pending}
			// A prior version retained completed checkpoints. Model a later setup
			// at another destination, while leaving the old owned key intact.
			must(t, local.Write(filepath.Join(home, ownKeyFile), checkpoint))
			current, _, err := config.Load(home)
			must(t, err)
			current.Storage.Bucket = "later-destination"
			current.MachineAssignment = nil
			must(t, config.Save(home, current))
			loaded, err := readOwnKeyCheckpoint(home, current)
			if pending {
				if err == nil {
					t.Fatal("incomplete old cleanup discarded")
				}
				var retained ownKeyCheckpoint
				must(t, local.Read(filepath.Join(home, ownKeyFile), &retained))
				if retained != checkpoint {
					t.Fatal("pending checkpoint changed")
				}
			} else if err != nil || loaded.SlotID != "" || loaded.DestinationID != current.DestinationID() {
				t.Fatal("completed old checkpoint blocked new destination", loaded, err)
			}
			if !providerKeyLive(cf, slots[0].ProviderID) || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
				t.Fatal("checkpoint retirement deleted prior provider key")
			}
		})
	}
}

func TestOwnKeyCompletedRetryPublishesWithoutMinting(t *testing.T) {
	env, home, cf, _, _ := ownKeyFixture(t)
	original := env.OpenStore
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		if cfg.MachineID != "" {
			return nil, errors.New("synthetic publication failure")
		}
		return original(cfg)
	}
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || !strings.Contains(output.String(), "registration pending") {
		t.Fatalf("committed publication failure: %d %s", code, output.String())
	}
	if _, err := os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
		t.Fatal("completed cleanup retained checkpoint")
	}
	store := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	// Retry committed publication independently, without another management token.
	env.LookupEnv = func(key string) (string, bool) { return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS" }
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	output.Reset()
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
		t.Fatalf("committed retry minted: %d %s", code, output.String())
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	raw, err := store.Get(t.Context(), "machines/"+cfg.MachineID+".json")
	must(t, err)
	if !bytes.Contains(raw, []byte(cfg.MachineAssignment.AccessKeyID)) {
		t.Fatal("completed retry did not publish committed ownership")
	}
}

// Ordinary setup can replace a shared key while an own-key migration is staged.
func TestOwnKeyStagedRetryRefusesChangedSharedReference(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(strconv.FormatBool(alias), func(t *testing.T) {
			env, home, cf, kc, cfg := ownKeyFixture(t)
			issuer := fixtureIssuer(t, env, home)
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
			slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
			must(t, err)
			replacement, err := kc.Load(t.Context(), cfg.Storage.R2CredentialRef)
			must(t, err)
			if !alias {
				replacement = credentials.R2Credentials{AccessKeyID: strings.Repeat("f", 32), SecretAccessKey: "replacement-shared-canary"}
			}
			next := cfg
			next.Storage.R2CredentialRef = "replacement-shared"
			next.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Shared, AccessKeyID: replacement.AccessKeyID, SharedWith: strings.Repeat("b", 32)}
			must(t, kc.Save(t.Context(), next.Storage.R2CredentialRef, replacement))
			userHome, err := env.userHomeDir()
			must(t, err)
			executable, err := env.executable()
			must(t, err)
			must(t, applySetup(home, userHome, executable, cfg, &next, nil, env))
			configBefore, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			checkpointBefore, err := os.ReadFile(filepath.Join(home, ownKeyFile))
			must(t, err)
			ledgerPath := filepath.Join(home, "issued", "slot-"+slot.SlotID+".json")
			ledgerBefore, err := os.ReadFile(ledgerPath)
			must(t, err)
			creates := cf.Calls(cloudflaretest.RouteCreateToken)
			deletes := cf.Calls(cloudflaretest.RouteDeleteToken)
			providerCalls, tokenReads, loads := 0, 0, 0
			provider := env.Cloudflare
			env.Cloudflare = func(token string) cloudflare.API {
				providerCalls++
				return provider(token)
			}
			env.UnsetEnv = func(string) error { tokenReads++; return nil }
			fakeSched(env).beforeLoad = func(scheduler.Ref) error { loads++; return nil }
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 1 || providerCalls != 0 || tokenReads != 0 || loads != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates || cf.Calls(cloudflaretest.RouteDeleteToken) != deletes {
				t.Fatalf("stale stage resumed after shared reference changed: code=%d provider=%d token=%d loads=%d output=%s", code, providerCalls, tokenReads, loads, output.String())
			}
			for _, file := range []struct {
				path   string
				before []byte
			}{{filepath.Join(home, "config.json"), configBefore}, {filepath.Join(home, ownKeyFile), checkpointBefore}, {ledgerPath, ledgerBefore}} {
				after, readErr := os.ReadFile(file.path)
				must(t, readErr)
				if !bytes.Equal(after, file.before) {
					t.Fatal("rejection changed committed config, checkpoint or staged ledger")
				}
			}
			stored, err := kc.Load(t.Context(), next.Storage.R2CredentialRef)
			must(t, err)
			if stored != replacement || !providerKeyLive(cf, slot.ProviderID) {
				t.Fatal("rejection lost replacement access or staged key")
			}
			// A changed shared reference blocks resume, but safe cancellation can
			// still remove the uncommitted staged key without consuming B or A.
			output.Reset()
			if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != deletes+1 || providerKeyLive(cf, slot.ProviderID) {
				t.Fatalf("safe stale-stage cancellation refused: %d %s", code, output.String())
			}
			stored, err = kc.Load(t.Context(), next.Storage.R2CredentialRef)
			must(t, err)
			if stored != replacement {
				t.Fatal("cancellation consumed replacement shared access")
			}
			if _, err = kc.Load(t.Context(), checkpoint.OldRef); err != nil {
				t.Fatal("cancellation consumed original shared access")
			}
			if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
				t.Fatal("safe cancelled checkpoint retained")
			}
		})
	}
}

func TestOwnKeyRecoversExactCommittedSlotBeforeCheckpointPromotion(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	next := cfg
	next.Storage.R2CredentialRef = slot.SecretRef
	next.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID}
	userHome, err := env.userHomeDir()
	must(t, err)
	executable, err := env.executable()
	must(t, err)
	must(t, applySetup(home, userHome, executable, cfg, &next, nil, env))
	// The setup commit succeeded, but the process stopped before marking its
	// checkpoint committed. Exact-slot recovery needs no management token.
	env.LookupEnv = func(key string) (string, bool) { return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS" }
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != creates || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatalf("exact committed slot recovery refused: %d %s", code, output.String())
	}
	slots, err := issuance.List(home)
	must(t, err)
	if len(slots) != 1 || slots[0].State != issuance.Own || !providerKeyLive(cf, slot.ProviderID) {
		t.Fatal("exact committed slot was not promoted and retained")
	}
	if _, err = kc.Load(t.Context(), checkpoint.OldRef); !errors.Is(err, credentials.ErrMissingCredential) {
		t.Fatal("exact committed-slot cleanup left obsolete shared secret")
	}
	if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
		t.Fatal("exact committed recovery did not retire completed checkpoint")
	}
}
