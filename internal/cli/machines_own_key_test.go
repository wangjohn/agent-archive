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
	"github.com/wangjohn/agent-archive/internal/platform"
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
		values := map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary}
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
	env.LookupEnv = func(string) (string, bool) { return "", false }
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
	env.LookupEnv = func(string) (string, bool) { return "", false }
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

func TestOwnKeyPreSlotCheckpointCanCancelAfterSetupChanges(t *testing.T) {
	for _, mode := range []struct {
		otherDestination bool
		cancel           bool
	}{{false, true}, {true, true}, {false, false}, {true, false}} {
		t.Run(strconv.FormatBool(mode.otherDestination)+"/cancel="+strconv.FormatBool(mode.cancel), func(t *testing.T) {
			env, home, cf, kc, cfg := ownKeyFixture(t)
			// The command durably writes this checkpoint before slot allocation.
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
			must(t, local.Write(filepath.Join(home, ownKeyFile), checkpoint))
			next := cfg
			if mode.otherDestination {
				next.Storage.Bucket = "replacement-destination"
			}
			next.Storage.R2CredentialRef = "replacement-shared"
			replacement := credentials.R2Credentials{AccessKeyID: strings.Repeat("f", 32), SecretAccessKey: "preslot-shared-canary"}
			must(t, kc.Save(t.Context(), next.Storage.R2CredentialRef, replacement))
			next.MachineAssignment = &config.MachineAssignment{DestinationID: next.DestinationID(), Kind: config.MachineAssignmentR2Shared, AccessKeyID: replacement.AccessKeyID, SharedWith: strings.Repeat("b", 32)}
			userHome, err := env.userHomeDir()
			must(t, err)
			executable, err := env.executable()
			must(t, err)
			must(t, applySetup(home, userHome, executable, cfg, &next, nil, env))
			before, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			providerCalls, tokenReads := 0, 0
			provider := env.Cloudflare
			env.Cloudflare = func(token string) cloudflare.API { providerCalls++; return provider(token) }
			env.UnsetEnv = func(string) error { tokenReads++; return nil }
			var output bytes.Buffer
			lookup := env.LookupEnv
			args := []string{"machines", "own-key", "--yes"}
			wantCode := 1
			if mode.cancel {
				args = append(args, "--cancel")
				wantCode = 0
			} else {
				// Stop a restart at token acquisition, after local retirement.
				env.LookupEnv = func(string) (string, bool) { return "", false }
			}
			if code := Run(args, nil, &output, &output, env); code != wantCode || providerCalls != 0 || tokenReads != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
				t.Fatalf("pre-slot cancellation blocked or called provider: %d %s", code, output.String())
			}
			after, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			stored, err := kc.Load(t.Context(), next.Storage.R2CredentialRef)
			must(t, err)
			if !bytes.Equal(before, after) || stored != replacement {
				t.Fatal("pre-slot cancellation changed current config or credential")
			}
			if _, err = kc.Load(t.Context(), checkpoint.OldRef); err != nil {
				t.Fatal("pre-slot cancellation removed old credential")
			}
			if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
				t.Fatal("pre-slot checkpoint retained")
			}
			// A later migration starts against the current destination/ref.
			env.Cloudflare = provider
			env.LookupEnv = lookup
			output.Reset()
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != 1 {
				t.Fatalf("new migration did not complete: %d %s", code, output.String())
			}
		})
	}
}

type preSlotProof string

const (
	preSlotCleanup              preSlotProof = "cleanup"
	preSlotCorruptLedger        preSlotProof = "corrupt-ledger"
	preSlotOrphanIntent         preSlotProof = "orphan-intent"
	preSlotSecretRemoval        preSlotProof = "secret-removal"
	preSlotIncompleteCheckpoint preSlotProof = "incomplete-checkpoint"
)

func TestOwnKeyPreSlotRetirementPreservesUncertainProof(t *testing.T) {
	for _, proof := range []preSlotProof{preSlotCleanup, preSlotCorruptLedger, preSlotOrphanIntent, preSlotSecretRemoval, preSlotIncompleteCheckpoint} {
		t.Run(string(proof), func(t *testing.T) {
			env, home, cf, _, cfg := ownKeyFixture(t)
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef, CleanupPending: proof == preSlotCleanup}
			must(t, local.Write(filepath.Join(home, ownKeyFile), checkpoint))
			if proof == preSlotIncompleteCheckpoint {
				must(t, os.WriteFile(filepath.Join(home, ownKeyFile), []byte("{}"), 0600))
			}
			if proof == preSlotCorruptLedger {
				must(t, os.MkdirAll(filepath.Join(home, "issued"), 0700))
				must(t, os.WriteFile(filepath.Join(home, "issued", "slot-broken.json"), []byte("broken"), 0600))
			}
			if proof == preSlotOrphanIntent || proof == preSlotSecretRemoval {
				issuer := fixtureIssuer(t, env, home)
				// Crash between durable creation intent and its owning callback.
				func() {
					defer func() {
						if recover() != "before-owning-callback" {
							t.Fatal("missing injected interruption")
						}
					}()
					_, _, _ = issuer.createWithIntent(issuance.Fresh, cfg.MachineID, func(issuance.Slot) error { panic("before-owning-callback") })
				}()
				if proof == preSlotSecretRemoval {
					slots, err := issuance.List(home)
					must(t, err)
					slot := slots[0]
					slot.State = issuance.Deleted
					slot.SecretRemovalPending = true
					must(t, issuance.Save(home, slot))
				}
			}
			before, err := os.ReadFile(filepath.Join(home, ownKeyFile))
			must(t, err)
			providerCalls := 0
			env.Cloudflare = func(string) cloudflare.API { providerCalls++; return nil }
			for _, args := range [][]string{{"machines", "own-key", "--cancel", "--yes"}, {"machines", "own-key", "--yes"}} {
				var output bytes.Buffer
				if code := Run(args, nil, &output, &output, env); code != 1 || providerCalls != 0 || cf.Calls(cloudflaretest.RouteCreateToken) != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
					t.Fatalf("uncertain pre-slot proof discarded: %d %s", code, output.String())
				}
				after, err := os.ReadFile(filepath.Join(home, ownKeyFile))
				must(t, err)
				if !bytes.Equal(before, after) {
					t.Fatal("uncertain checkpoint changed")
				}
			}
		})
	}
}

func TestOwnKeyDeletedStageSecretRemovalCannotBeForgotten(t *testing.T) {
	env, home, cf, kc, cfg := ownKeyFixture(t)
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	slot.State = issuance.CleanupPending
	must(t, issuance.Save(home, slot))
	issuer.env.Credentials = func() (credentials.CredentialStore, error) {
		return &failingDeleteKeychain{fakeKeychain: kc, deleteErr: credentials.ErrKeychainLocked}, nil
	}
	must(t, issuer.cleanupOwnKeyStage(&slot))
	if slot.State != issuance.Deleted || !slot.SecretRemovalPending {
		t.Fatal("synthetic secret cleanup failure did not persist")
	}
	creates := cf.Calls(cloudflaretest.RouteCreateToken)
	if _, err = ownKeySlot(home, &checkpoint, issuer, issuer.api, env); err == nil || checkpoint.SlotID != slot.SlotID || cf.Calls(cloudflaretest.RouteCreateToken) != creates {
		t.Fatal("deleted stage lost pending secret or minted replacement")
	}
	var output bytes.Buffer
	if code := cancelStagedOwnKey(home, checkpoint, issuer, &output, &output, env); code != 1 {
		t.Fatal("cancellation discarded secret removal obligation")
	}
	var retained ownKeyCheckpoint
	must(t, local.Read(filepath.Join(home, ownKeyFile), &retained))
	if retained != checkpoint {
		t.Fatal("pending secret checkpoint changed")
	}
}

func TestOwnKeyPreSlotCancellationDoesNotCancelCompletedOwnership(t *testing.T) {
	env, home, cf, _, cfg := ownKeyFixture(t)
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
		t.Fatal("synthetic migration failed", output.String())
	}
	slots, err := issuance.List(home)
	must(t, err)
	// A legacy completed journal can be retired on retry, but is not a
	// pre-slot interruption that cancellation may report as cancelled.
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef, SlotID: slots[0].SlotID, Committed: true}
	must(t, local.Write(filepath.Join(home, ownKeyFile), checkpoint))
	output.Reset()
	if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 1 || cf.Calls(cloudflaretest.RouteCreateToken) != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, slots[0].ProviderID) {
		t.Fatalf("completed ownership treated as cancelled pre-slot: %d %s", code, output.String())
	}
}

type ownKeySecretRetryStore struct {
	*fakeKeychain
	fail        bool
	deleteRefs  []string
	afterDelete func(string) error
}

func (s *ownKeySecretRetryStore) LoadStored(ctx context.Context, ref string) (credentials.R2Credentials, error) {
	key, err := s.Load(ctx, ref)
	if errors.Is(err, credentials.ErrMissingCredential) {
		return key, credentials.ErrKeychainItemNotFound
	}
	return key, err
}

func (s *ownKeySecretRetryStore) Delete(ctx context.Context, ref string) error {
	s.deleteRefs = append(s.deleteRefs, ref)
	if s.fail {
		return credentials.ErrKeychainLocked
	}
	if err := s.fakeKeychain.Delete(ctx, ref); err != nil {
		return err
	}
	if s.afterDelete != nil {
		return s.afterDelete(ref)
	}
	return nil
}

func deletedOwnKeySecretFixture(t *testing.T) (Env, string, *cloudflaretest.Server, *ownKeySecretRetryStore, config.Config, issuance.Slot) {
	t.Helper()
	env, home, cf, kc, cfg := ownKeyFixture(t)
	store := &ownKeySecretRetryStore{fakeKeychain: kc}
	env.Credentials = func() (credentials.CredentialStore, error) { return store, nil }
	issuer := fixtureIssuer(t, env, home)
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
	must(t, err)
	slot.State = issuance.CleanupPending
	must(t, issuance.Save(home, slot))
	store.fail = true
	must(t, issuer.cleanupOwnKeyStage(&slot))
	if slot.State != issuance.Deleted || !slot.SecretRemovalPending || providerKeyLive(cf, slot.ProviderID) {
		t.Fatal("fixture did not confirm deletion and retain secret cleanup")
	}
	return env, home, cf, store, cfg, slot
}

func TestOwnKeyDeletedStageSecretRemovalRetriesAfterStoreRecovery(t *testing.T) {
	for _, cancelStage := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelStage), func(t *testing.T) {
			env, home, cf, store, cfg, oldSlot := deletedOwnKeySecretFixture(t)
			configBefore, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			store.fail = false
			beforeDeletes := len(store.deleteRefs)
			args := []string{"machines", "own-key", "--yes"}
			wantCreates := 2
			if cancelStage {
				args = append(args, "--cancel")
				wantCreates = 1
			}
			var output bytes.Buffer
			if code := Run(args, nil, &output, &output, env); code != 0 {
				t.Fatalf("recovered secret cleanup still blocked: %d %s", code, output.String())
			}
			if cf.Calls(cloudflaretest.RouteDeleteToken) != 1 || cf.Calls(cloudflaretest.RouteCreateToken) != wantCreates {
				t.Fatal("cleanup repeated provider deletion or minted on cancellation")
			}
			if len(store.deleteRefs) <= beforeDeletes || store.deleteRefs[beforeDeletes] != oldSlot.SecretRef {
				t.Fatal("retry did not remove only the exact staged secret first")
			}
			if _, err = store.Load(t.Context(), oldSlot.SecretRef); !errors.Is(err, credentials.ErrMissingCredential) {
				t.Fatal("recovered staged secret remains")
			}
			slots, err := issuance.List(home)
			must(t, err)
			for _, slot := range slots {
				if slot.SlotID == oldSlot.SlotID && (slot.State != issuance.Deleted || slot.SecretRemovalPending) {
					t.Fatal("secret cleanup success did not durably clear exact deleted slot")
				}
			}
			if _, err = os.Stat(filepath.Join(home, ownKeyFile)); !os.IsNotExist(err) {
				t.Fatal("completed retry retained checkpoint")
			}
			if cancelStage {
				after, err := os.ReadFile(filepath.Join(home, "config.json"))
				must(t, err)
				if !bytes.Equal(configBefore, after) || len(store.deleteRefs) != beforeDeletes+1 {
					t.Fatal("cleanup-only cancellation changed config or removed another secret")
				}
				if _, err = store.Load(t.Context(), cfg.Storage.R2CredentialRef); err != nil {
					t.Fatal("cleanup cancellation removed shared access")
				}
			} else {
				current, _, err := config.Load(home)
				must(t, err)
				if current.MachineAssignment == nil || current.MachineAssignment.Kind != config.MachineAssignmentR2Own || len(slots) != 2 {
					t.Fatal("resumed migration did not finish new ownership")
				}
			}
		})
	}
}

func TestOwnKeyDeletedStageSecretRetryPreservesBindingGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *config.Config, *ownKeySecretRetryStore, *issuance.Slot)
	}{
		{"active-alias", func(t *testing.T, cfg *config.Config, store *ownKeySecretRetryStore, slot *issuance.Slot) {
			t.Helper()
			key, err := store.Load(t.Context(), slot.SecretRef)
			must(t, err)
			must(t, store.Save(t.Context(), cfg.Storage.R2CredentialRef, key))
		}},
		{"retained-alias", func(t *testing.T, cfg *config.Config, store *ownKeySecretRetryStore, slot *issuance.Slot) {
			t.Helper()
			key, err := store.Load(t.Context(), slot.SecretRef)
			must(t, err)
			must(t, store.Save(t.Context(), "retained-stage-alias", key))
			previous := cfg.Storage
			previous.Bucket = "retained-destination"
			previous.R2CredentialRef = "retained-stage-alias"
			cfg.PreviousDestinations = []credentials.Config{previous}
		}},
		{"retired-reference", func(_ *testing.T, cfg *config.Config, _ *ownKeySecretRetryStore, slot *issuance.Slot) {
			cfg.RetiredCredentialRefs = []string{slot.SecretRef}
		}},
		{"retired-alias", func(t *testing.T, cfg *config.Config, store *ownKeySecretRetryStore, slot *issuance.Slot) {
			t.Helper()
			key, err := store.Load(t.Context(), slot.SecretRef)
			must(t, err)
			must(t, store.Save(t.Context(), "retired-stage-alias", key))
			cfg.RetiredCredentialRefs = []string{"retired-stage-alias"}
		}},
		{"unknown-retired-binding", func(_ *testing.T, cfg *config.Config, _ *ownKeySecretRetryStore, _ *issuance.Slot) {
			cfg.RetiredCredentialRefs = []string{"unknown-retired"}
		}},
		{"changed-staged-secret", func(t *testing.T, _ *config.Config, store *ownKeySecretRetryStore, slot *issuance.Slot) {
			t.Helper()
			must(t, store.Save(t.Context(), slot.SecretRef, credentials.R2Credentials{AccessKeyID: strings.Repeat("f", 32), SecretAccessKey: "changed-secret-canary"}))
		}},
		{"unknown-provider-identity", func(_ *testing.T, _ *config.Config, _ *ownKeySecretRetryStore, slot *issuance.Slot) {
			slot.ProviderID = ""
		}},
		{"unconfirmed-delete-reason", func(_ *testing.T, _ *config.Config, _ *ownKeySecretRetryStore, slot *issuance.Slot) {
			slot.CleanupReason = "creation-refused"
		}},
		{"changed-destination", func(_ *testing.T, cfg *config.Config, _ *ownKeySecretRetryStore, _ *issuance.Slot) {
			cfg.Storage.Bucket = "later-destination"
			cfg.MachineAssignment = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, home, cf, store, cfg, slot := deletedOwnKeySecretFixture(t)
			store.fail = false
			tc.mutate(t, &cfg, store, &slot)
			must(t, config.Save(home, cfg))
			path := filepath.Join(home, "issued", "slot-"+slot.SlotID+".json")
			// Direct write models incomplete identity proof, which lifecycle Save
			// deliberately refuses to manufacture by changing immutable fields.
			must(t, local.Write(path, slot))
			before, err := os.ReadFile(path)
			must(t, err)
			deletes := len(store.deleteRefs)
			for _, args := range [][]string{{"machines", "own-key", "--yes"}, {"machines", "own-key", "--cancel", "--yes"}} {
				var output bytes.Buffer
				if code := Run(args, nil, &output, &output, env); code != 1 || len(store.deleteRefs) != deletes || cf.Calls(cloudflaretest.RouteDeleteToken) != 1 || cf.Calls(cloudflaretest.RouteCreateToken) != 1 {
					t.Fatalf("secret retry bypassed retained/unknown binding: %d %s", code, output.String())
				}
				after, err := os.ReadFile(path)
				must(t, err)
				if !bytes.Equal(before, after) {
					t.Fatal("refused secret retry altered issuance proof")
				}
				if _, err = os.Stat(filepath.Join(home, ownKeyFile)); err != nil {
					t.Fatal("refused secret retry lost checkpoint")
				}
			}
		})
	}
}

func TestDeletedOwnKeySecretRetryKeepsProofWhenSuccessCannotBeJournaled(t *testing.T) {
	env, home, cf, store, _, slot := deletedOwnKeySecretFixture(t)
	store.fail = false
	issuer := fixtureIssuer(t, env, home)
	path := filepath.Join(home, "issued", "slot-"+slot.SlotID+".json")
	backup := filepath.Join(home, "pending-secret-proof.json")
	store.afterDelete = func(ref string) error {
		if ref != slot.SecretRef {
			t.Fatal("cleanup attempted unrelated secret")
		}
		// Inject a journal-write failure after successful local deletion.
		return os.Rename(path, backup)
	}
	if err := issuer.retryDeletedOwnKeySecret(&slot); err == nil || !slot.SecretRemovalPending {
		t.Fatal("unpersisted secret cleanup success discarded pending proof")
	}
	var retained issuance.Slot
	must(t, local.Read(backup, &retained))
	if !retained.SecretRemovalPending {
		t.Fatal("journal failure cleared persisted pending proof")
	}
	must(t, os.Rename(backup, path))
	store.afterDelete = nil
	var output bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--cancel", "--yes"}, nil, &output, &output, env); code != 0 || cf.Calls(cloudflaretest.RouteDeleteToken) != 1 || cf.Calls(cloudflaretest.RouteCreateToken) != 1 {
		t.Fatalf("retry of already absent exact secret failed: %d %s", code, output.String())
	}
	slots, err := issuance.List(home)
	must(t, err)
	if len(slots) != 1 || slots[0].SecretRemovalPending {
		t.Fatal("absence recovery did not durably clear pending proof")
	}
}

func TestOwnKeyStageRequiresStoredCredentialDespiteMatchingEnvironment(t *testing.T) {
	for _, missingFile := range []bool{false, true} {
		t.Run(strconv.FormatBool(missingFile), func(t *testing.T) {
			env, home, cf, oldStore, cfg := ownKeyFixture(t)
			oldKey, err := oldStore.Load(t.Context(), cfg.Storage.R2CredentialRef)
			must(t, err)
			shellKey := credentials.R2Credentials{}
			lookup := func(name string) (string, bool) {
				values := map[string]string{
					credentials.EnvR2AccessKeyID:     shellKey.AccessKeyID,
					credentials.EnvR2SecretAccessKey: shellKey.SecretAccessKey,
				}
				value, ok := values[name]
				return value, ok && value != ""
			}
			dir := credentials.FileStoreDir(home)
			store, err := credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return dir, nil }, LookupEnv: lookup})
			must(t, err)
			must(t, store.Save(t.Context(), cfg.Storage.R2CredentialRef, oldKey))
			env.Credentials = func() (credentials.CredentialStore, error) { return store, nil }
			issuer := fixtureIssuer(t, env, home)
			checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
			slot, err := ownKeySlot(home, &checkpoint, issuer, issuer.api, env)
			must(t, err)
			shellKey, err = credentials.LoadStored(t.Context(), store, slot.SecretRef)
			must(t, err)
			if missingFile {
				must(t, store.Delete(t.Context(), slot.SecretRef))
				// This actual Linux store can still answer Load from the injected
				// matching shell key, while its persisted slot is absent.
				loaded, err := store.Load(t.Context(), slot.SecretRef)
				must(t, err)
				if loaded != shellKey {
					t.Fatal("fixture did not expose matching environment fallback")
				}
				if _, err = credentials.LoadStored(t.Context(), store, slot.SecretRef); !errors.Is(err, credentials.ErrCredentialFileNotFound) {
					t.Fatal("fixture still has persisted stage credential")
				}
			}
			configBefore, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			checkpointBefore, err := os.ReadFile(filepath.Join(home, ownKeyFile))
			must(t, err)
			ledgerPath := filepath.Join(home, "issued", "slot-"+slot.SlotID+".json")
			ledgerBefore, err := os.ReadFile(ledgerPath)
			must(t, err)
			loads := 0
			fakeSched(env).beforeLoad = func(scheduler.Ref) error { loads++; return nil }
			wantCode := 0
			if missingFile {
				wantCode = 1
			}
			var output bytes.Buffer
			if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != wantCode {
				t.Fatalf("stored-stage boundary: missing=%v code=%d output=%s", missingFile, code, output.String())
			}
			if cf.Calls(cloudflaretest.RouteCreateToken) != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || !providerKeyLive(cf, slot.ProviderID) {
				t.Fatal("stage retry minted or deleted a provider key")
			}
			if missingFile {
				if loads != 0 {
					t.Fatal("missing persisted stage reached setup commit")
				}
				for _, file := range []struct {
					path   string
					before []byte
				}{{filepath.Join(home, "config.json"), configBefore}, {filepath.Join(home, ownKeyFile), checkpointBefore}, {ledgerPath, ledgerBefore}} {
					after, err := os.ReadFile(file.path)
					must(t, err)
					if !bytes.Equal(after, file.before) {
						t.Fatal("missing persisted stage changed config or durable stage")
					}
				}
				stored, err := credentials.LoadStored(t.Context(), store, cfg.Storage.R2CredentialRef)
				must(t, err)
				if stored != oldKey {
					t.Fatal("missing persisted stage lost old shared stored access")
				}
				// Persisting the original exact key allows normal reuse/commit.
				must(t, store.Save(t.Context(), slot.SecretRef, shellKey))
				output.Reset()
				if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &output, env); code != 0 {
					t.Fatal("restored persisted stage did not resume", output.String())
				}
			}
			current, _, err := config.Load(home)
			must(t, err)
			withoutShell, err := credentials.OpenDefault(credentials.OpenOptions{OS: platform.Linux, Dir: func() (string, error) { return dir, nil }, LookupEnv: func(string) (string, bool) { return "", false }})
			must(t, err)
			persisted, err := credentials.LoadStored(t.Context(), withoutShell, current.Storage.R2CredentialRef)
			must(t, err)
			if current.MachineAssignment == nil || current.MachineAssignment.Kind != config.MachineAssignmentR2Own || persisted != shellKey || cf.Calls(cloudflaretest.RouteCreateToken) != 1 || cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
				t.Fatal("persisted stage was not committed/reused independently of shell credentials")
			}
		})
	}
}
