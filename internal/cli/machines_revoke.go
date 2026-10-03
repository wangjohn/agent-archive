package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/revocation"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type revokeSelector struct {
	Name          string
	MachineID     string
	RecipientID   string
	PairingID     string
	IncludeIssued bool
	BindingFile   string
}

func runMachinesRevoke(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	opts, code := parseRevokeOptions(args, errOut, env)
	if code != 0 {
		return code
	}
	selector := opts.Selector
	if env.getenv("AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_REVOKE") != "1" {
		return machineCommandError(errOut, errors.New("revocation is experimental; set AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_REVOKE=1"))
	}
	home, err := env.readHome()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	release, err := lockRevocation(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer release()
	cfg, found, err := config.Load(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if !found {
		return machineCommandError(errOut, errNotSetUp)
	}
	journal, err := prepareRevocation(home, cfg, selector, opts.Retry, env)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if opts.Retry != "" && journal.RequestOnly {
		return machineCommandError(errOut, errors.New("request-only operation has no verified selection; start a new explicit revocation"))
	}
	if opts.Retry != "" && journal.Complete() {
		if err = reconcileRevocationSlots(home, journal); err != nil {
			return machineCommandError(errOut, err)
		}
		publishRevocation(context.Background(), home, &journal, cfg, env)
		printRevocation(out, journal, opts.JSON)
		return 0
	}
	p := newPrompter(stdin, out)
	interactive := !opts.Yes && !opts.JSON && env.interactive(stdin)
	token := ""
	tokenErr := errors.New("AWS profile is not a unique principal")
	if cfg.Storage.Provider != credentials.ProviderS3 {
		token, _, _, tokenErr = readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, interactive)
	}
	if tokenErr != nil || cfg.Storage.Provider == credentials.ProviderS3 {
		journal.RequestOnly = len(journal.Keys) == 0
		journal.PublicationPending = true
		if err = revocation.Save(home, journal); err != nil {
			return machineCommandError(errOut, err)
		}
		publishRevocation(context.Background(), home, &journal, cfg, env)
		printRevocation(out, journal, opts.JSON)
		printUnverifiedRevocationClaims(errOut, cfg, selector)
		terminal.Println(errOut, "Revocation requested—access not removed. Claimed keys are unverified; use the provider dashboard or IAM permissions.")
		if cfg.Storage.Provider == credentials.ProviderS3 {
			terminal.Println(errOut, "AWS access not removed; change IAM permissions. A profile is not a unique principal.")
		} else {
			terminal.Println(errOut, cloudflare.TokenDashboardURL)
		}
		return 1
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	reader, ok := api.(cloudflare.InventoryAPI)
	if !ok {
		return machineCommandError(errOut, errors.New("provider cannot verify token metadata"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	if opts.Retry == "" {
		journal, err = selectRevocation(ctx, home, cfg, selector, journal, api, reader, env)
		if err != nil {
			return machineCommandError(errOut, err)
		}
		terminal.Printf(errOut, "Verified selection: %d key(s), recipient/issuer lineage only. Account visibility is incomplete or unknown.\n", len(journal.Keys))
		for _, key := range journal.Keys {
			terminal.Printf(errOut, "%s recipient %s issuer %s slot %s\n", key.ProviderID, key.RecipientID, key.IssuerID, key.SlotID)
		}
		if err = confirmRevocation(p, opts.Yes, interactive); err != nil {
			return machineCommandError(errOut, err)
		}
		if err = revocation.Save(home, journal); err != nil {
			return machineCommandError(errOut, err)
		}
	}
	cancel()
	if err = reconcileRevocationSlots(home, journal); err != nil {
		return machineCommandError(errOut, err)
	}
	// Informational publication has its own budget, outside provider execution.
	publishRevocation(context.Background(), home, &journal, cfg, env)
	executionCtx, executionCancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer executionCancel()
	executionErr := executeRevocation(executionCtx, home, &journal, cfg, api, reader, env)
	printRevocation(out, journal, opts.JSON)
	if executionErr != nil {
		terminal.Println(errOut, "Operation interrupted or local persistence failed; retain confirmed outcomes and retry the exact operation.")
		return 1
	}
	if !journal.Complete() {
		return 1
	}
	return 0
}

func lockRevocation(home string) (func(), error) {
	var releases []func()
	release := func() {
		for n := len(releases) - 1; n >= 0; n-- {
			releases[n]()
		}
	}
	for _, name := range []string{"revocations.lock", "setup.lock", "issued.lock"} {
		unlock, err := local.NamedLock(home, name)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, unlock)
	}
	if setupjournal.TransactionPending(home) {
		release()
		return nil, errors.New("setup recovery pending; run setup before revocation")
	}
	return release, nil
}

func confirmRevocation(p *prompter, yes, interactive bool) error {
	if yes {
		return nil
	}
	if !interactive {
		return errors.New("use an interactive terminal or deliberate --yes after reviewing verified ownership")
	}
	consent, err := p.yesNo("Delete this exact verified key set? Sessions remain; other recipients may lose access", false)
	if err != nil || !consent {
		return errors.New("revocation cancelled; no keys deleted")
	}
	return nil
}

func prepareRevocation(home string, cfg config.Config, selector revokeSelector, retry string, env Env) (revocation.Journal, error) {
	if retry != "" {
		j, err := revocation.Load(home, retry)
		if err == nil && (j.DestinationID != cfg.DestinationID() || j.RequesterID != cfg.MachineID) {
			err = errors.New("operation belongs to another local destination or requester")
		}
		if err == nil && cfg.Storage.Provider != credentials.ProviderS3 {
			account, bucket, destinationErr := providerDestination(cfg.Storage)
			if destinationErr != nil || j.AccountID != account || j.Bucket != bucket.Name || j.Jurisdiction != bucket.Jurisdiction {
				err = errors.New("operation provider scope does not match the committed destination")
			}
		}
		return j, err
	}
	id, err := local.ID()
	if err != nil {
		return revocation.Journal{}, err
	}
	account, bucket, err := providerDestination(cfg.Storage)
	if err != nil && cfg.Storage.Provider != credentials.ProviderS3 {
		return revocation.Journal{}, err
	}
	j := revocation.Journal{Version: 1, OperationID: id, DestinationID: cfg.DestinationID(), RequesterID: cfg.MachineID, RequestedSelector: requestedRevocationSelector(selector), AccountID: account, Bucket: bucket.Name, Jurisdiction: bucket.Jurisdiction, CreatedAt: env.now().UTC(), IncludeIssued: selector.IncludeIssued, Keys: []revocation.Key{}, PublicationPending: true}
	return j, j.Validate()
}

func requestedRevocationSelector(selector revokeSelector) *revocation.RequestedSelector {
	for _, requested := range []revocation.RequestedSelector{
		{Kind: revocation.RequestedName, Value: selector.Name},
		{Kind: revocation.RequestedMachineID, Value: selector.MachineID},
		{Kind: revocation.RequestedRecipientID, Value: selector.RecipientID},
		{Kind: revocation.RequestedPairingID, Value: selector.PairingID},
	} {
		if requested.Value != "" {
			return &requested
		}
	}
	return nil
}

func publishRevocation(parent context.Context, home string, j *revocation.Journal, cfg config.Config, env Env) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), machines.Timeout)
	defer cancel()
	store, err := env.openStoreContext(ctx, cfg)
	if err == nil {
		err = putRevocation(ctx, store, *j)
	}
	j.PublicationPending = err != nil
	_ = revocation.Save(home, *j)
}

func putRevocation(ctx context.Context, store storage.ObjectStore, j revocation.Journal) error {
	if err := j.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return store.Put(ctx, "machines/revocations/"+j.OperationID+".json", raw)
}

func executeRevocation(ctx context.Context, home string, j *revocation.Journal, cfg config.Config, api cloudflare.API, reader cloudflare.InventoryAPI, env Env) error {
	// Per-key results are durable locally. Publish the final snapshot separately
	// so slow bucket writes cannot consume the provider deadline between keys.
	defer publishRevocation(ctx, home, j, cfg, env)
	for _, key := range j.Keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if key.Outcome == revocation.Confirmed {
			continue
		}
		outcome := deleteVerifiedRevocationKey(ctx, j, key, api, reader)
		j.Record(key.ProviderID, outcome)
		if err := revocation.Save(home, *j); err != nil {
			return err
		}
		if err := reconcileRevocationSlots(home, *j); err != nil {
			return err
		}
	}
	return nil
}

// Caller holds issued.lock through selection, confirmation and execution.
// Withhold selected local slots before deletion, including ambiguous outcomes;
// retry can finish ledger retirement from a durable confirmed journal alone.
func reconcileRevocationSlots(home string, j revocation.Journal) error {
	slots, err := issuance.List(home)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		for _, key := range j.Keys {
			if slot.ProviderID != key.ProviderID || slot.State == issuance.Deleted {
				continue
			}
			if slot.DestinationID != j.DestinationID || slot.AccountID != j.AccountID || slot.Bucket != j.Bucket || slot.Jurisdiction != j.Jurisdiction || slot.PermissionID != j.PermissionID || slot.RecipientID != key.RecipientID || slot.IssuerID != key.IssuerID || slot.SlotID != key.SlotID {
				return errors.New("selected local issuance binding changed; revocation paused")
			}
			slot.State = issuance.CleanupPending
			slot.CleanupReason = "explicit-revocation"
			if err = issuance.Save(home, slot); err != nil {
				return err
			}
			if key.Outcome == revocation.Confirmed {
				slot.State = issuance.Deleted
				slot.CleanupReason = "provider-delete-confirmed"
				slot.SecretRemovalPending = true
				if err = issuance.Save(home, slot); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func revocationMetadataMatches(token cloudflare.TokenMetadata, key revocation.Key, j revocation.Journal) bool {
	id, ok := cloudflare.ParseProviderName(token.Name)
	return ok && token.ID == key.ProviderID && id.RecipientID == key.RecipientID && id.IssuerID == key.IssuerID && id.SlotID == key.SlotID && cloudflare.ExactBucketPolicy(token, j.AccountID, cloudflare.BucketRef{Name: j.Bucket, Jurisdiction: j.Jurisdiction}, j.PermissionID)
}

func printRevocation(out io.Writer, j revocation.Journal, asJSON bool) {
	if asJSON {
		_ = json.NewEncoder(out).Encode(j)
		return
	}
	terminal.Printf(out, "Revocation operation %s\n", j.OperationID)
	if j.RequestOnly && j.RequestedSelector != nil {
		terminal.Printf(out, "Unverified requested %s: %s\n", j.RequestedSelector.Kind, j.RequestedSelector.Value)
	}

	for _, key := range j.Keys {
		terminal.Printf(out, "%s: %s\n", key.ProviderID, key.Outcome)
	}
	if j.Complete() {
		terminal.Println(out, "Access removed for the verified key set; immediate cutoff is not guaranteed.")
	} else {
		terminal.Println(out, "Access not verified removed; pending or failed/unknown keys remain.")
	}
	if j.PublicationPending {
		terminal.Println(out, "Bucket progress publication pending; local provider results are retained.")
	}
	terminal.Println(out, "Account inventory completeness remains unknown. Sessions and downloaded data remain.")
}

type operatorBinding struct {
	MachineID             string                   `json:"machine_id"`
	Assignment            config.MachineAssignment `json:"assignment"`
	IndependentlyVerified bool                     `json:"independently_verified"`
}

func selectRevocation(ctx context.Context, home string, cfg config.Config, selector revokeSelector, j revocation.Journal, api cloudflare.API, reader cloudflare.InventoryAPI, env Env) (revocation.Journal, error) {
	slots, err := issuance.List(home)
	if err != nil {
		return j, err
	}
	groups, err := api.PermissionGroups(ctx, j.AccountID, cloudflare.PermissionBucketItemWrite)
	if err != nil {
		return j, errors.New("provider permission evidence unavailable")
	}
	permission, err := cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite)
	if err != nil {
		return j, err
	}
	j.PermissionID = permission
	machineID, recipientID, assignment, err := trustedRevokeTarget(ctx, cfg, selector, slots, env)
	if err != nil {
		return j, err
	}
	j.TargetID = machineID
	expected := map[string]revocation.Key{}
	if assignment != nil {
		if assignment.Kind != config.MachineAssignmentR2Own || !config.ValidMachineID(assignment.AccessKeyID) || !config.ValidMachineID(assignment.SlotID) {
			return j, errors.New("shared or legacy ownership cannot be independently revoked; migrate with machines own-key")
		}
		expected[assignment.AccessKeyID] = revocation.Key{ProviderID: assignment.AccessKeyID, RecipientID: assignment.RecipientID, IssuerID: assignment.IssuerID, SlotID: assignment.SlotID, Outcome: revocation.Pending}
	}
	addLedgerRevocationKeys(expected, slots, cfg, j, machineID, recipientID)
	if selector.IncludeIssued {
		if machineID == "" {
			return j, errors.New("--include-issued requires independently verified immutable machine identity")
		}
		inventory, e := reader.TokenInventory(ctx, j.AccountID)
		if e != nil || !inventory.PaginationComplete {
			return j, errors.New("issuer inventory incomplete; no deletion started")
		}
		for _, token := range inventory.Tokens {
			id, known := cloudflare.ParseProviderName(token.Name)
			if known && id.IssuerID == machineID {
				if !cloudflare.ExactBucketPolicy(token, j.AccountID, cloudflare.BucketRef{Name: j.Bucket, Jurisdiction: j.Jurisdiction}, permission) {
					return j, errors.New("issuer descendant scope unknown; no deletion started")
				}
				expected[token.ID] = revocation.Key{ProviderID: token.ID, RecipientID: id.RecipientID, IssuerID: id.IssuerID, SlotID: id.SlotID, Outcome: revocation.Pending}
			}
		}
	}
	if len(expected) == 0 || len(expected) > 128 {
		return j, errors.New("no bounded independently verified key set; no deletion started")
	}
	for _, key := range expected {
		metadata, e := reader.TokenDetails(ctx, j.AccountID, key.ProviderID)
		if e != nil || !revocationMetadataMatches(metadata, key, j) {
			return j, errors.New("provider key missing, not visible, or binding mismatched; no deletion started")
		}
		j.Keys = append(j.Keys, key)
	}
	active := ""
	if cfg.MachineAssignment != nil && cfg.MachineAssignment.DestinationID == cfg.DestinationID() {
		active = cfg.MachineAssignment.AccessKeyID
	}
	sort.Slice(j.Keys, func(a, b int) bool {
		if j.Keys[a].ProviderID == active {
			return false
		}
		if j.Keys[b].ProviderID == active {
			return true
		}
		return j.Keys[a].ProviderID < j.Keys[b].ProviderID
	})
	return j, j.Validate()
}

func trustedRevokeTarget(ctx context.Context, cfg config.Config, selector revokeSelector, slots []issuance.Slot, env Env) (string, string, *config.MachineAssignment, error) {
	machineID := selector.MachineID
	if selector.Name != "" {
		store, err := env.openStoreContext(ctx, cfg)
		if err != nil {
			return "", "", nil, errors.New("cannot read target hints")
		}
		listing := machines.List(ctx, store)
		if listing.Partial || len(listing.Unreadable) > 0 {
			return "", "", nil, errors.New("target listing incomplete; use trusted recipient or pairing ID")
		}
		record, err := machines.Select(listing.Records, selector.Name)
		if err != nil {
			return "", "", nil, err
		}
		machineID = record.MachineID
		// A bucket writer can replace this machine's label. Its immutable local
		// assignment does not authorize a different name supplied by the caller.
		localName := cfg.MachineName
		if localName == "" && config.ValidMachineID(cfg.MachineID) {
			localName = "unnamed-" + cfg.MachineID[:4]
		}
		if machineID == cfg.MachineID && selector.Name != localName && selector.Name != cfg.MachineID {
			return "", "", nil, errors.New("bucket label does not match this machine's trusted local name; use an independently verified immutable ID")
		}
	}
	if selector.BindingFile != "" {
		proof, err := readOperatorBinding(selector.BindingFile, cfg.DestinationID())
		if err != nil {
			return "", "", nil, err
		}
		if machineID != "" && machineID != proof.MachineID || selector.RecipientID != "" && selector.RecipientID != proof.Assignment.RecipientID || selector.PairingID != "" && selector.PairingID != proof.Assignment.PairingID {
			return "", "", nil, errors.New("operator binding does not match the selected immutable identity")
		}
		return proof.MachineID, proof.Assignment.RecipientID, &proof.Assignment, nil
	}
	if machineID == cfg.MachineID && cfg.MachineAssignment != nil && cfg.MachineAssignment.DestinationID == cfg.DestinationID() {
		return machineID, cfg.MachineAssignment.RecipientID, cfg.MachineAssignment, nil
	}
	return trustedLedgerRecipient(cfg, selector, slots)
}

func readOperatorBinding(path, destination string) (operatorBinding, error) {
	var proof operatorBinding
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return proof, errors.New("operator binding must be a private regular file verified independently of bucket records")
	}
	f, err := os.Open(path)
	if err != nil {
		return proof, errors.New("cannot read independent operator binding")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 {
		return proof, errors.New("operator binding exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proof) != nil || !proof.IndependentlyVerified || !config.ValidMachineID(proof.MachineID) || proof.Assignment.DestinationID != destination || (config.Config{MachineAssignment: &proof.Assignment}).ValidateMachine() != nil {
		return proof, errors.New("invalid independent operator binding; never copy bucket claims into this file")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return proof, errors.New("operator binding has trailing data")
	}
	return proof, nil
}

func deleteVerifiedRevocationKey(ctx context.Context, j *revocation.Journal, key revocation.Key, api cloudflare.API, reader cloudflare.InventoryAPI) revocation.Outcome {
	metadata, err := reader.TokenDetails(ctx, j.AccountID, key.ProviderID)
	if err != nil || !revocationMetadataMatches(metadata, key, *j) {
		return revocation.Unknown
	}
	if err = api.DeleteToken(ctx, j.AccountID, key.ProviderID); err != nil {
		return revocation.Unknown
	}
	return revocation.Confirmed
}

type revokeOptions struct {
	Selector revokeSelector
	Retry    string
	Yes      bool
	JSON     bool
}

func parseRevokeOptions(args []string, errOut io.Writer, env Env) (revokeOptions, int) {
	fs := env.newCommandFlags("machines revoke", errOut)
	proof := fs.String("binding-file", "", "private independently verified operator binding, never a bucket record")
	machine := fs.String("machine-id", "", "select an independently bound machine ID")
	recipient := fs.String("recipient-id", "", "select a trusted recipient ID")
	pair := fs.String("pairing-id", "", "select a trusted issuance pairing ID")
	retry := fs.String("operation-id", "", "resume exactly this local operation")
	include := fs.Bool("include-issued", false, "also check visible provider issuer descendants, including delivered keys")
	yes := fs.Bool("yes", false, "do not prompt; unresolved ownership still refuses")
	asJSON := fs.Bool("json", false, "write secret-free per-key outcomes")
	name, haveName, parsed := fs.parseWithOptionalArgument(args)
	if !parsed {
		return revokeOptions{}, 2
	}
	count := 0
	if haveName {
		count++
	}
	for _, id := range []string{*machine, *recipient, *pair, *retry} {
		if id != "" {
			count++
			if !config.ValidMachineID(id) {
				return revokeOptions{}, fs.usageError("use a canonical full ID")
			}
		}
	}
	if count != 1 || (*retry != "" && *include) {
		return revokeOptions{}, fs.usageError("choose exactly one NAME, --machine-id, --recipient-id, --pairing-id, or --operation-id")
	}
	return revokeOptions{Selector: revokeSelector{Name: name, MachineID: *machine, RecipientID: *recipient, PairingID: *pair, IncludeIssued: *include, BindingFile: *proof}, Retry: *retry, Yes: *yes, JSON: *asJSON}, 0
}

func addLedgerRevocationKeys(expected map[string]revocation.Key, slots []issuance.Slot, cfg config.Config, j revocation.Journal, machineID, recipientID string) {
	for _, slot := range slots {
		if slot.DestinationID != cfg.DestinationID() || slot.AccountID != j.AccountID || slot.Bucket != j.Bucket || slot.Jurisdiction != j.Jurisdiction || slot.PermissionID != j.PermissionID || slot.ProviderID == "" || slot.State == issuance.Deleted {
			continue
		}
		belongs := slot.RecipientID == recipientID
		ownOrSpare := machineID != "" && slot.IssuerID == machineID && (slot.State == issuance.Own || slot.State == issuance.Spare)
		if belongs || ownOrSpare {
			expected[slot.ProviderID] = revocation.Key{ProviderID: slot.ProviderID, RecipientID: slot.RecipientID, IssuerID: slot.IssuerID, SlotID: slot.SlotID, Outcome: revocation.Pending}
		}
	}
}

func trustedLedgerRecipient(cfg config.Config, selector revokeSelector, slots []issuance.Slot) (string, string, *config.MachineAssignment, error) {
	recipient := selector.RecipientID
	for _, slot := range slots {
		// Only this healthy local issuer's persisted lineage establishes a recipient.
		if slot.DestinationID != cfg.DestinationID() || slot.IssuerID != cfg.MachineID || slot.State == issuance.Deleted {
			continue
		}
		match := selector.PairingID != "" && slot.PairingID == selector.PairingID || recipient != "" && slot.RecipientID == recipient
		if match {
			if recipient != "" && recipient != slot.RecipientID {
				return "", "", nil, errors.New("ambiguous trusted recipient")
			}
			recipient = slot.RecipientID
		}
	}
	if recipient != "" {
		for _, slot := range slots {
			if slot.DestinationID == cfg.DestinationID() && slot.IssuerID == cfg.MachineID && slot.RecipientID == recipient && slot.State != issuance.Deleted {
				return "", recipient, nil, nil
			}
		}
	}
	return "", "", nil, errors.New("bucket mapping is untrusted; provide local committed ownership, healthy issuer recipient/pairing lineage, or an independently verified --binding-file; --yes cannot bypass this")
}

func printUnverifiedRevocationClaims(out io.Writer, cfg config.Config, selector revokeSelector) {
	assignment := cfg.MachineAssignment
	if assignment == nil || assignment.DestinationID != cfg.DestinationID() || !config.ValidMachineID(assignment.AccessKeyID) {
		return
	}
	selected := selector.MachineID == cfg.MachineID || selector.Name != "" && selector.Name == cfg.MachineName || selector.RecipientID != "" && selector.RecipientID == assignment.RecipientID || selector.PairingID != "" && selector.PairingID == assignment.PairingID
	if selected {
		terminal.Printf(out, "Claimed key %s (unverified; no access removal confirmed).\n", assignment.AccessKeyID)
	}
}
