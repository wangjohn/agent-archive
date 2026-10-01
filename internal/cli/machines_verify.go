package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type machineProviderObservation struct {
	MachineID   string `json:"machine_id"`
	AccessKeyID string `json:"access_key_id,omitempty"`
	State       string `json:"state"`
	Binding     string `json:"binding"`
}

type providerVerification struct {
	CheckedAt                time.Time                    `json:"checked_at"`
	PaginationComplete       bool                         `json:"pagination_complete"`
	AccountInventoryComplete bool                         `json:"account_inventory_complete"`
	Visibility               string                       `json:"visibility"`
	Partial                  bool                         `json:"partial"`
	Diagnostic               string                       `json:"diagnostic,omitempty"`
	Observations             []machineProviderObservation `json:"observations"`
	Unclaimed                []string                     `json:"claim_not_observed,omitempty"`
}

type verifiedMachinesResult struct {
	machines.ListResult
	Verification providerVerification `json:"verification"`
}

func providerDestination(cfg credentials.Config) (string, cloudflare.BucketRef, error) {
	var ref cloudflare.BucketRef
	if cfg.Provider != credentials.ProviderR2 || cloudflare.ValidateBucketName(cfg.Bucket) != nil {
		return "", ref, errors.New("provider verification requires a Cloudflare R2 bucket")
	}
	endpoint := cfg.R2Endpoint
	if endpoint == "" {
		endpoint = cloudflare.Endpoint(cfg.R2AccountID, "")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Port() != "" {
		return "", ref, errors.New("provider verification refuses this storage endpoint")
	}
	host := strings.Split(u.Host, ".")
	if len(host) != 4 && len(host) != 5 {
		return "", ref, errors.New("provider verification requires a Cloudflare endpoint")
	}
	account := host[0]
	if !config.ValidMachineID(account) || (cfg.R2AccountID != "" && cfg.R2AccountID != account) {
		return "", ref, errors.New("provider account does not match the configured endpoint")
	}
	jurisdiction := ""
	if len(host) == 5 {
		jurisdiction = host[1]
		if !cloudflare.ValidJurisdiction(jurisdiction) {
			return "", ref, errors.New("unsupported Cloudflare jurisdiction")
		}
	}
	if strings.TrimSuffix(endpoint, "/") != cloudflare.Endpoint(account, jurisdiction) {
		return "", ref, errors.New("provider verification refuses this storage endpoint")
	}
	return account, cloudflare.BucketRef{Name: cfg.Bucket, Jurisdiction: jurisdiction}, nil
}

func runMachinesVerify(cfg config.Config, listing machines.ListResult, stdin io.Reader, out, errOut io.Writer, env Env, asJSON, unattended bool) int {
	if env.getenv("AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY") != "1" {
		return machineCommandError(errOut, errors.New("provider verification is experimental; set AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY=1 after reviewing its limitations"))
	}
	account, bucket, err := providerDestination(cfg.Storage)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	p := newPrompter(stdin, errOut)
	interactive := !unattended && !asJSON && env.interactive(stdin)
	if interactive {
		terminal.Println(errOut, "Explicit read-only provider check. Management token stays in memory; use Account API Tokens Read or Write.")
		terminal.Println(errOut, cloudflare.TokenDashboardURL)
	}
	token, _, _, err := readManagementToken(context.Background(), p, env, cfg.CloudflareTokenCommand, interactive)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	api := env.cloudflareAPI(token)
	defer api.Discard()
	inventoryAPI, ok := api.(cloudflare.InventoryAPI)
	if !ok {
		return machineCommandError(errOut, errors.New("provider client cannot read token inventory"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudflare.InventoryTimeout)
	defer cancel()
	report := verifyProvider(ctx, cfg, listing, api, inventoryAPI, account, bucket, env.now())
	result := verifiedMachinesResult{ListResult: listing, Verification: report}
	result.ProviderVerified = !report.Partial && report.PaginationComplete
	if asJSON {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return machineCommandError(errOut, err)
		}
	} else {
		terminal.Printf(out, "Provider check at %s.\n", report.CheckedAt.Format(time.RFC3339))
		for _, observation := range report.Observations {
			terminal.Printf(out, "%s  %s  %s (%s)\n", observation.MachineID, observation.AccessKeyID, observation.State, observation.Binding)
		}
		for _, id := range report.Unclaimed {
			terminal.Printf(out, "Provider key %s: claim not observed.\n", id)
		}
		if report.Diagnostic != "" {
			terminal.Printf(out, "Provider check incomplete: %s.\n", report.Diagnostic)
		}
		terminal.Println(out, "Account inventory completeness is unknown: the token may list only keys it created. Not observed can mean missing or not visible.")
		terminal.Println(out, "Bucket claims are untrusted; matching provider metadata alone does not establish machine ownership or access removal.")
	}
	if report.Partial || listing.Partial || len(listing.Unreadable) > 0 {
		return 1
	}
	return 0
}

func verifyProvider(ctx context.Context, cfg config.Config, listing machines.ListResult, api cloudflare.API, reader cloudflare.InventoryAPI, account string, bucket cloudflare.BucketRef, now time.Time) providerVerification {
	report := providerVerification{CheckedAt: now.UTC(), Visibility: "unknown_may_be_creator_only", Observations: []machineProviderObservation{}}
	inventory, err := reader.TokenInventory(ctx, account)
	report.PaginationComplete = inventory.PaginationComplete
	if err != nil {
		report.Partial = true
		report.Diagnostic = "token_listing_failed_or_incomplete"
	}
	tokens := map[string]cloudflare.TokenMetadata{}
	for _, token := range inventory.Tokens {
		tokens[token.ID] = token
	}
	groups, groupErr := api.PermissionGroups(ctx, account, cloudflare.PermissionBucketItemWrite)
	permissionID := ""
	if groupErr == nil {
		permissionID, groupErr = cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite)
	}
	if groupErr != nil {
		report.Partial = true
		report.Diagnostic = "permission_evidence_unavailable"
	}
	claimed := map[string]bool{}
	details := 0
	for _, record := range listing.Records {
		binding := record.Credential
		observation := machineProviderObservation{MachineID: record.MachineID, AccessKeyID: binding.AccessKeyID, State: "legacy_or_unknown_binding", Binding: "untrusted_bucket_claim"}
		claimed[binding.AccessKeyID] = true
		if !config.ValidMachineID(binding.AccessKeyID) {
			observation.AccessKeyID = ""
			report.Observations = append(report.Observations, observation)
			continue
		}
		token, found := tokens[binding.AccessKeyID]
		if (!found || len(token.Policies) == 0) && details < 16 && ctx.Err() == nil {
			details++
			detail, detailErr := reader.TokenDetails(ctx, account, binding.AccessKeyID)
			if detailErr == nil {
				token = detail
				found = true
				tokens[token.ID] = token
			}
		}
		observation.State, found = providerBindingState(token, found, binding, account, bucket, permissionID, now)
		if !found {
			report.Partial = true
		}

		if record.MachineID == cfg.MachineID && cfg.MachineAssignment != nil && cfg.MachineAssignment.DestinationID == cfg.DestinationID() {
			local := cfg.MachineAssignment
			if local.AccessKeyID == binding.AccessKeyID && local.RecipientID == binding.RecipientID && local.IssuerID == binding.IssuerID && local.SlotID == binding.SlotID && local.Kind == binding.Kind {
				observation.Binding = "local_committed_binding"
			} else {
				observation.State = "local_binding_mismatch"
				report.Partial = true
			}
		}
		report.Observations = append(report.Observations, observation)
	}
	for _, token := range inventory.Tokens {
		if _, known := cloudflare.ParseProviderName(token.Name); known && !claimed[token.ID] && cloudflare.ExactBucketPolicy(token, account, bucket, permissionID) {
			report.Unclaimed = append(report.Unclaimed, token.ID)
		}
	}
	if ctx.Err() != nil {
		report.Partial = true
		report.Diagnostic = "provider_budget_exhausted"
	}
	return report
}

func providerBindingState(token cloudflare.TokenMetadata, found bool, binding machines.CredentialBinding, account string, bucket cloudflare.BucketRef, permissionID string, now time.Time) (string, bool) {
	if !found {
		return "missing_or_not_visible", false
	}
	if !cloudflare.ExactBucketPolicy(token, account, bucket, permissionID) {
		return "scope_unknown_or_mismatch", false
	}
	if !providerTokenActive(token, now) {
		return "provider_key_not_active", false
	}
	if binding.Kind != "r2_own" {
		return "legacy_or_unknown_binding", true
	}
	id, known := cloudflare.ParseProviderName(token.Name)
	if !known || id.RecipientID != binding.RecipientID || id.IssuerID != binding.IssuerID || id.SlotID != binding.SlotID {
		return "issuance_unknown_or_mismatch", false
	}
	return "provider_metadata_matches_claim", true
}

func providerTokenActive(token cloudflare.TokenMetadata, now time.Time) bool {
	if token.Status != "active" {
		return false
	}
	if token.ExpiresOn != "" {
		expiry, err := time.Parse(time.RFC3339, token.ExpiresOn)
		if err != nil || !now.Before(expiry) {
			return false
		}
	}
	if token.NotBefore != "" {
		start, err := time.Parse(time.RFC3339, token.NotBefore)
		if err != nil || now.Before(start) {
			return false
		}
	}
	return true
}
