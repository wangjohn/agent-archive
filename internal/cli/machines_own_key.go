package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const ownKeyFile = "own-key.json"

type ownKeyCheckpoint struct {
	DestinationID  string `json:"destination_id"`
	OldRef         string `json:"old_ref"`
	SlotID         string `json:"slot_id,omitempty"`
	Committed      bool   `json:"committed"`
	CleanupPending bool   `json:"cleanup_pending"`
}

func runMachinesOwnKey(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	fs := env.newCommandFlags("machines own-key", errOut)
	yes := fs.Bool("yes", false, "use an environment management token without prompting")
	cancelOwn := fs.Bool("cancel", false, "cancel a proven uncommitted dedicated-key stage")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if env.getenv("AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS") != "1" {
		return machineCommandError(errOut, errors.New("own-key is experimental; set AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS=1"))
	}
	if err := pairingAgentRefusal(env); err != nil {
		return machineCommandError(errOut, err)
	}
	home, err := env.readHome()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer release()
	cfg, err := ownKeyConfiguration(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	issuedRelease, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer issuedRelease()
	checkpoint, err := readOwnKeyCheckpoint(home, cfg)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if checkpoint.Committed || checkpoint.SlotID != "" && cfg.Storage.R2CredentialRef == "issued-"+checkpoint.SlotID {
		if *cancelOwn {
			return machineCommandError(errOut, errors.New("dedicated key already committed; cancellation refused"))
		}
		checkpoint.Committed = true
		return finishOwnKey(home, checkpoint, out, errOut, env)
	}
	if cfg.MachineAssignment != nil && cfg.MachineAssignment.DestinationID == cfg.DestinationID() && cfg.MachineAssignment.Kind == config.MachineAssignmentR2Own && checkpoint.SlotID == "" {
		terminal.Println(out, "This machine already has locally committed dedicated ownership; no key created.")
		return 0
	}
	p := newPrompter(stdin, out)
	interactive := !*yes && env.interactive(stdin)
	if err = confirmOwnKey(p, *yes, interactive); err != nil {
		return machineCommandError(errOut, err)
	}
	token, _, _, err := readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, interactive)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	issuer, err := newKeyIssuer(home, cfg, env, p, api)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if *cancelOwn {
		return cancelStagedOwnKey(home, checkpoint, issuer, out, errOut, env)
	}
	if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
		return machineCommandError(errOut, err)
	}
	slot, err := ownKeySlot(home, &checkpoint, issuer, api, env)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	next := cfg
	next.Storage.R2CredentialRef = slot.SecretRef
	next.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID}
	if err = next.ValidateMachine(); err != nil {
		return machineCommandError(errOut, err)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	executable, err := env.executable()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if err = applySetup(home, userHome, executable, cfg, &next, nil, env); err != nil {
		return machineCommandError(errOut, errors.New("dedicated key remains staged; setup did not commit, retry own-key after recovery; shared access unchanged"))
	}
	checkpoint.Committed = true
	checkpoint.CleanupPending = true
	if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
		return machineCommandError(errOut, errors.New("dedicated key committed; cleanup checkpoint pending, retry own-key"))
	}
	return finishOwnKey(home, checkpoint, out, errOut, env)
}

func readOwnKeyCheckpoint(home string, cfg config.Config) (ownKeyCheckpoint, error) {
	checkpoint := ownKeyCheckpoint{DestinationID: cfg.DestinationID(), OldRef: cfg.Storage.R2CredentialRef}
	path := filepath.Join(home, ownKeyFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return checkpoint, nil
	}
	if err := local.Read(path, &checkpoint); err != nil {
		return checkpoint, errors.New("own-key checkpoint unreadable; no new key created")
	}
	if checkpoint.DestinationID != cfg.DestinationID() || checkpoint.SlotID != "" && !config.ValidMachineID(checkpoint.SlotID) || !config.SafeMachineText(checkpoint.OldRef, 128) {
		return checkpoint, errors.New("own-key checkpoint does not match this destination")
	}
	return checkpoint, nil
}

func ownKeySlot(home string, checkpoint *ownKeyCheckpoint, issuer *keyIssuer, api cloudflare.API, env Env) (issuance.Slot, error) {
	var slot issuance.Slot
	if checkpoint.SlotID == "" {
		created, _, err := issuer.createWithIntent(issuance.Fresh, issuer.cfg.MachineID, func(intent issuance.Slot) error {
			checkpoint.SlotID = intent.SlotID
			checkpoint.CleanupPending = true
			return local.Write(filepath.Join(home, ownKeyFile), checkpoint)
		})
		if err != nil {
			return slot, err
		}
		slot = created
		checkpoint.SlotID = slot.SlotID
		checkpoint.CleanupPending = true
		if err = local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
			return slot, errors.New("own key staged; checkpoint write failed, inspect issuance before retrying")
		}
	} else {
		slots, err := issuance.List(home)
		if err != nil {
			return slot, err
		}
		for _, candidate := range slots {
			if candidate.SlotID == checkpoint.SlotID {
				slot = candidate
				break
			}
		}
		if slot.SlotID == "" || slot.DestinationID != issuer.cfg.DestinationID() || slot.IssuerID != issuer.cfg.MachineID || slot.RecipientID != issuer.cfg.MachineID {
			return slot, errors.New("own-key staged lineage mismatched")
		}
	}
	if slot.State == issuance.CreationIntent || slot.State == issuance.SecretIntent || slot.State == issuance.CleanupPending {
		release, err := lockCollector(home, "own-key recovery cleanup", env.now())
		if err != nil {
			return slot, err
		}
		err = issuer.cleanupOwnKeyStage(&slot)
		release()
		if err != nil {
			return slot, err
		}
	}
	if slot.State == issuance.Deleted {
		checkpoint.SlotID = ""
		if err := local.Write(filepath.Join(home, ownKeyFile), checkpoint); err != nil {
			return slot, err
		}
		return ownKeySlot(home, checkpoint, issuer, api, env)
	}
	if slot.State != issuance.OwnIntent {
		return slot, errors.New("own-key creation or cleanup remains uncertain; no duplicate key created")
	}
	reader, ok := api.(cloudflare.InventoryAPI)
	if !ok {
		return slot, errors.New("provider cannot verify dedicated ownership")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	metadata, err := reader.TokenDetails(ctx, issuer.account, slot.ProviderID)
	if err != nil || metadata.ID != slot.ProviderID || metadata.Name != slot.ProviderName || !cloudflare.ExactBucketPolicy(metadata, issuer.account, issuer.bucket, issuer.group) {
		return slot, errors.New("staged dedicated metadata not independently verified")
	}
	kc, err := env.credentialStore()
	if err != nil {
		return slot, err
	}
	key, err := kc.Load(ctx, slot.SecretRef)
	if err != nil || key.AccessKeyID != slot.ProviderID {
		return slot, errors.New("staged dedicated credential missing")
	}
	verifier := r2Creator{env: env, p: issuer.p, account: issuer.account, bucket: cloudflare.BucketSpec{BucketRef: issuer.bucket}}
	if err = verifier.checkKey(ctx, key); err != nil {
		return slot, errors.New("staged dedicated key did not pass storage check; previous credential unchanged")
	}
	return slot, nil
}

func finishOwnKey(home string, checkpoint ownKeyCheckpoint, out, errOut io.Writer, env Env) int {
	// Commit has completed. Only now may the old local secret be removed.
	release, err := lockCollector(home, "own-key cleanup", env.now())
	if err != nil {
		return machineCommandError(errOut, err)
	}
	hooksRelease, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		release()
		return machineCommandError(errOut, err)
	}
	current, found, err := config.Load(home)
	if err != nil || !found || current.Storage.R2CredentialRef != "issued-"+checkpoint.SlotID || current.DestinationID() != checkpoint.DestinationID {
		hooksRelease()
		release()
		return machineCommandError(errOut, errors.New("committed own-key destination changed; old access retained"))
	}
	if err = promoteCommittedOwnKey(home, current, checkpoint); err != nil {
		hooksRelease()
		release()
		return machineCommandError(errOut, err)
	}
	retained := oldSharedAccessNeeded(current, checkpoint.OldRef, env)
	checkpoint.Committed = true
	checkpoint.CleanupPending = retained
	if !retained && checkpoint.OldRef != "" && checkpoint.OldRef != current.Storage.R2CredentialRef {
		kc, e := env.credentialStore()
		if e == nil {
			e = kc.Delete(context.Background(), checkpoint.OldRef)
		}
		checkpoint.CleanupPending = e != nil
		if e == nil {
			current.RetiredCredentialRefs = slices.DeleteFunc(current.RetiredCredentialRefs, func(ref string) bool { return ref == checkpoint.OldRef })
			err = config.Save(home, current)
		}
	}
	if err == nil {
		err = local.Write(filepath.Join(home, ownKeyFile), checkpoint)
	}
	hooksRelease()
	release()
	if err != nil {
		return machineCommandError(errOut, errors.New("dedicated key committed; old access cleanup pending"))
	}
	terminal.Println(out, "Dedicated key committed through setup's transaction. The shared provider key was not deleted.")
	if checkpoint.CleanupPending {
		terminal.Println(out, "Old shared access remains locally present or needed by another local destination; cleanup pending.")
	} else {
		terminal.Println(out, "Old shared local secret removed after commit.")
	}
	if err = publishMachineAfterSetup(home, env); err != nil {
		terminal.Println(out, "Machine registration pending; committed dedicated ownership is retained.")
	}
	return 0
}

// References can alias the same provider credential. Preserve truthful retained
// access reporting, and keep cleanup pending when another binding is unknown.
func oldSharedAccessNeeded(cfg config.Config, oldRef string, env Env) bool {
	if oldRef == "" {
		return false
	}
	for _, destination := range cfg.PreviousDestinations {
		if destination.Provider != credentials.ProviderR2 {
			continue
		}
		if destination.R2CredentialRef == oldRef {
			return true
		}
		kc, err := env.credentialStore()
		if err != nil {
			return true
		}
		old, err := credentials.LoadStored(context.Background(), kc, oldRef)
		if err != nil || old.AccessKeyID == "" {
			return true
		}
		other, err := credentials.LoadStored(context.Background(), kc, destination.R2CredentialRef)
		if err != nil || other.AccessKeyID == "" || other.AccessKeyID == old.AccessKeyID {
			return true
		}
	}
	return false
}

// Caller holds issued.lock and the collector lock, so config and ledger proof
// remain stable across the final promotion. A crash before this step leaves
// OwnIntent ineligible for spare delivery or ordinary automatic cleanup.
func promoteCommittedOwnKey(home string, cfg config.Config, checkpoint ownKeyCheckpoint) error {
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		if slot.SlotID != checkpoint.SlotID {
			continue
		}
		assignment := cfg.MachineAssignment
		if assignment == nil || assignment.Kind != config.MachineAssignmentR2Own || assignment.DestinationID != checkpoint.DestinationID || assignment.SlotID != slot.SlotID || assignment.AccessKeyID != slot.ProviderID || assignment.RecipientID != cfg.MachineID || assignment.IssuerID != cfg.MachineID || slot.RecipientID != cfg.MachineID || slot.IssuerID != cfg.MachineID || slot.DestinationID != checkpoint.DestinationID || cfg.Storage.R2CredentialRef != slot.SecretRef {
			return errors.New("committed own-key lineage mismatch; old access retained")
		}
		if slot.State == issuance.Own {
			return nil
		}
		if slot.State != issuance.OwnIntent {
			return errors.New("dedicated key is not a verified own intent; old access retained")
		}
		slot.State = issuance.Own
		return issuance.Save(home, slot)
	}
	return errors.New("committed own-key issuance missing; old access retained")
}

func cancelStagedOwnKey(home string, checkpoint ownKeyCheckpoint, issuer *keyIssuer, out, errOut io.Writer, env Env) int {
	if checkpoint.SlotID == "" {
		return machineCommandError(errOut, errors.New("no staged own-key operation to cancel"))
	}
	release, err := lockCollector(home, "own-key cancellation", env.now())
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer release()
	slots, err := issuance.List(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	for _, slot := range slots {
		if slot.SlotID != checkpoint.SlotID {
			continue
		}
		if slot.DestinationID != checkpoint.DestinationID || slot.IssuerID != issuer.cfg.MachineID || slot.RecipientID != issuer.cfg.MachineID {
			return machineCommandError(errOut, errors.New("staged ownership mismatch; cancellation refused"))
		}
		switch slot.State {
		case issuance.OwnIntent:
			err = issuer.cleanupUncommittedOwnIntent(&slot)
		case issuance.CreationIntent, issuance.SecretIntent, issuance.CleanupPending:
			err = issuer.cleanupOwnKeyStage(&slot)
		case issuance.Spare, issuance.Reserved, issuance.DeliveryIntent, issuance.Delivered, issuance.Own:
			return machineCommandError(errOut, errors.New("slot is not owned by an uncommitted own-key transaction; cancellation refused"))
		case issuance.Deleted:
			// A confirmed prior cleanup only needs its checkpoint removed.
		}
		if err != nil || slot.State != issuance.Deleted {
			return machineCommandError(errOut, errors.New("staged cancellation remains pending; shared access unchanged"))
		}
		if err = os.Remove(filepath.Join(home, ownKeyFile)); err != nil {
			return machineCommandError(errOut, err)
		}
		terminal.Println(out, "Uncommitted dedicated key removed. Shared provider and local access retained.")
		return 0
	}
	return machineCommandError(errOut, errors.New("staged issuance unavailable; cancellation refused"))
}

func ownKeyConfiguration(home string) (config.Config, error) {
	if setupjournal.TransactionPending(home) {
		return config.Config{}, errors.New("setup recovery pending; run setup before own-key")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	if !found {
		return config.Config{}, errNotSetUp
	}
	if _, _, err = providerDestination(cfg.Storage); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func confirmOwnKey(p *prompter, yes, interactive bool) error {
	if yes {
		return nil
	}
	if !interactive {
		return errors.New("own-key needs an interactive terminal or deliberate --yes")
	}
	consent, err := p.yesNo("Create and commit a dedicated key? The shared provider key stays valid for other users", false)
	if err != nil || !consent {
		return errors.New("own-key cancelled; nothing changed")
	}
	return nil
}

// cleanupUncommittedOwnIntent is only for an owning transaction whose durable
// journal proves this slot was never exposed before its config commit. The
// caller holds issued.lock and serializes config writers with collector.lock.
func (i *keyIssuer) cleanupUncommittedOwnIntent(s *issuance.Slot) error {
	if s.State != issuance.OwnIntent {
		return errors.New("own transaction cleanup requires own-intent")
	}
	if err := i.ownKeyCleanupSafe(*s); err != nil {
		return err
	}
	s.State = issuance.CleanupPending
	s.CleanupReason = "uncommitted-own-transaction"
	if err := issuance.Save(i.home, *s); err != nil {
		return err
	}
	i.cleanup(s)
	if s.State != issuance.Deleted {
		return errors.New("own-key cleanup pending; provider access may remain")
	}
	return nil
}

// Every recovery cleanup rechecks all committed local destinations under the
// collector lock, even after a crash has advanced the stage to CleanupPending.
func (i *keyIssuer) ownKeyCleanupSafe(s issuance.Slot) error {
	if setupjournal.TransactionPending(i.home) {
		return errors.New("setup recovery pending; own-key cleanup refused")
	}
	cfg, found, err := config.Load(i.home)
	if err != nil || !found || cfg.MachineID != s.IssuerID || cfg.DestinationID() != s.DestinationID {
		return errors.New("committed ownership unavailable; own-key cleanup pending")
	}
	if cfg.Storage.R2CredentialRef == s.SecretRef || (cfg.MachineAssignment != nil && cfg.MachineAssignment.AccessKeyID == s.ProviderID) {
		return errors.New("committed own-key binding retained; cleanup refused")
	}
	if cfg.Storage.Provider != credentials.ProviderR2 || cfg.Storage.R2CredentialRef == "" {
		return errors.New("active credential binding unavailable; own-key cleanup pending")
	}
	kc, err := i.env.credentialStore()
	if err != nil {
		return errors.New("active credential unavailable; own-key cleanup pending")
	}
	for _, destination := range append([]credentials.Config{cfg.Storage}, cfg.PreviousDestinations...) {
		if destination.Provider != credentials.ProviderR2 {
			continue
		}
		if destination.R2CredentialRef == "" || destination.R2CredentialRef == s.SecretRef {
			return errors.New("retained credential binding unavailable or uses staged key; cleanup refused")
		}
		active, loadErr := credentials.LoadStored(context.Background(), kc, destination.R2CredentialRef)
		if loadErr != nil || active.AccessKeyID == "" {
			return errors.New("retained credential unavailable; own-key cleanup pending")
		}
		if s.ProviderID != "" && active.AccessKeyID == s.ProviderID {
			return errors.New("active own-key binding retained; cleanup refused")
		}
	}
	return nil
}

// An uncertain creation may have no recorded provider ID. Resolve and persist
// that exact slot first, then compare every active/retained credential before
// cleanup. Generic orphan cleanup alone cannot rule out an aliased active key.
func (i *keyIssuer) cleanupOwnKeyStage(s *issuance.Slot) error {
	if s.ProviderID == "" {
		reader, ok := i.api.(cloudflare.InventoryAPI)
		if !ok {
			return errors.New("staged provider identity unavailable; own-key cleanup pending")
		}
		ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
		defer cancel()
		inventory, err := reader.TokenInventory(ctx, i.account)
		if err != nil || !inventory.PaginationComplete {
			return errors.New("staged provider inventory incomplete; own-key cleanup pending")
		}
		matches := 0
		for _, token := range inventory.Tokens {
			if token.Name == s.ProviderName {
				if !config.ValidMachineID(token.ID) || !cloudflare.ExactBucketPolicy(token, i.account, i.bucket, s.PermissionID) {
					return errors.New("staged provider scope mismatched; own-key cleanup refused")
				}
				s.ProviderID = token.ID
				matches++
			}
		}
		if matches != 1 {
			return errors.New("staged provider identity ambiguous; own-key cleanup pending")
		}
		if err = issuance.Save(i.home, *s); err != nil {
			return err
		}
	}
	if err := i.ownKeyCleanupSafe(*s); err != nil {
		return err
	}
	i.cleanup(s)
	return nil
}
