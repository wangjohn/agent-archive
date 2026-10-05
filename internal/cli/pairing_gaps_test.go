package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/revocation"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestMachineRemoteFakeAcceptance(t *testing.T) {
	for _, mode := range []string{"source-name", "third-rename", "re-pair-retired", "issuer-compromise", "partial-retry", "concurrent-source", "revoke-repair-revoke"} {
		t.Run(mode, func(t *testing.T) {
			source, sourceHome, cf, _, _ := ownKeyFixture(t)
			store := storagetest.NewMemoryStore()
			source.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
			lookup := source.LookupEnv
			source.LookupEnv = func(k string) (string, bool) {
				if value, ok := map[string]string{"AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_REVOKE": "1", "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY": "1"}[k]; ok {
					return value, true
				}
				return lookup(k)
			}
			run := func(env Env, input string, args ...string) string {
				t.Helper()
				var out, diagnostics bytes.Buffer
				if code := Run(args, strings.NewReader(input), &out, &diagnostics, env); code != 0 {
					t.Fatalf("%v: %d %s %s", args, code, &out, &diagnostics)
				}
				return out.String()
			}
			run(source, "", "machines", "own-key", "--yes")
			receiverHome, userHome := t.TempDir(), t.TempDir()
			receiver := setupTestEnv(t, receiverHome, userHome, newFakeKeychain(), source.now())
			receiver.OpenStore = source.OpenStore
			receiver.Cloudflare = source.Cloudflare
			receiver.DetectHarnesses = func(string) []string { return []string{"codex"} }
			receiver.UnsetEnv = func(string) error { return nil }
			project := filepath.Join(userHome, "project")
			must(t, os.MkdirAll(project, 0700))
			pair := func(name string) {
				t.Helper()
				delivery := run(source, "", "machines", "add", "--name", name, "--spares=1", "--yes")
				bundle, code := "", ""
				for line := range strings.SplitSeq(delivery, "\n") {
					if strings.HasPrefix(line, "aa-pair1:") {
						bundle = line
					}
					if value, ok := strings.CutPrefix(line, "Pairing code (deliver separately): "); ok {
						code = value
					}
				}
				if bundle == "" || code == "" {
					t.Fatal("missing delivery")
				}
				receiver.LookupEnv = func(k string) (string, bool) {
					if k == "AGENT_ARCHIVE_PAIRING_CODE" {
						return code, true
					}
					return source.LookupEnv(k)
				}
				output := run(receiver, bundle, "setup", "--pair-file", "-", "--yes", "--codex-discovery", "off", "--codex-capture-scope", "included-projects", "--project", project)
				if strings.Index(output, "Paired with") > strings.Index(output, "Configuration saved") {
					t.Fatal("pair success appears after housekeeping")
				}
			}
			pair("laptop")
			cfg, _, err := config.Load(receiverHome)
			must(t, err)
			original := *cfg.MachineAssignment
			if mode == "source-name" {
				run(source, "", "machines", "revoke", "laptop", "--yes")
				if providerKeyLive(cf, original.AccessKeyID) {
					t.Fatal("issuer name did not revoke recipient")
				}
				return
			}
			if mode == "revoke-repair-revoke" {
				run(receiver, "", "machines", "revoke", "--machine-id", cfg.MachineID, "--yes")
				if providerKeyLive(cf, original.AccessKeyID) {
					t.Fatal("first deletion was not confirmed")
				}
				pair("replacement")
				cfg, _, err = config.Load(receiverHome)
				must(t, err)
				if len(cfg.RetiredMachineAssignments) != 1 || cfg.RetiredMachineAssignments[0].AccessKeyID != original.AccessKeyID {
					t.Fatal("confirmed retired history was discarded")
				}
				receiver.Cloudflare = func(token string) cloudflare.API {
					api := source.Cloudflare(token)
					return absentRetiredTokenAPI{API: api, InventoryAPI: api.(cloudflare.InventoryAPI), retiredID: original.AccessKeyID}
				}
				run(receiver, "", "machines", "revoke", "--machine-id", cfg.MachineID, "--yes", "--json")
				if providerKeyLive(cf, cfg.MachineAssignment.AccessKeyID) {
					t.Fatal("confirmed retired key blocked replacement revocation")
				}
				operations, err := revocation.List(receiverHome)
				must(t, err)
				if len(operations) != 2 {
					t.Fatal("replacement operation was not retained")
				}
				for _, operation := range operations {
					if len(operation.Keys) != 1 || !operation.Complete() {
						t.Fatal("replacement selected an already-confirmed key")
					}
				}
				return
			}
			if mode == "concurrent-source" {
				unattended := source
				unattended.LookupEnv = func(k string) (string, bool) {
					if k == "CLOUDFLARE_API_TOKEN" {
						return "", false
					}
					return source.LookupEnv(k)
				}
				type result struct {
					code   int
					output string
				}
				completed := make(chan result, 2)
				for _, name := range []string{"concurrent-one", "concurrent-two"} {
					go func() {
						var out bytes.Buffer
						code := Run([]string{"machines", "add", "--name", name, "--yes"}, nil, &out, &out, unattended)
						completed <- result{code, out.String()}
					}()
				}
				winners := 0
				for range 2 {
					r := <-completed
					if r.code == 0 {
						winners++
					}
				}
				if winners != 1 {
					t.Fatal("concurrent source commands did not reserve the sole spare exactly once")
				}
				slots, err := issuance.List(sourceHome)
				must(t, err)
				observed := 0
				for _, slot := range slots {
					if strings.HasPrefix(slot.Label, "concurrent-") && slot.State == issuance.Delivered {
						observed++
					}
				}
				if observed != 1 {
					t.Fatal("concurrent delivery duplicated or lost lineage")
				}
			}
			if mode == "re-pair-retired" {
				pair("laptop-again")
				cfg, _, err = config.Load(receiverHome)
				must(t, err)
				if len(cfg.RetiredMachineAssignments) != 1 || cfg.RetiredMachineAssignments[0].AccessKeyID != original.AccessKeyID || !providerKeyLive(cf, original.AccessKeyID) {
					t.Fatal("re-pair lost retired committed assignment")
				}
			}
			run(receiver, "", "machines", "rename", "portable")
			cfg, _, err = config.Load(receiverHome)
			must(t, err)
			issuer := fixtureIssuer(t, receiver, receiverHome)
			spare, _, err := issuer.create(issuance.Precreated)
			must(t, err)
			delivered, _, err := issuer.create(issuance.Precreated)
			must(t, err)
			delivered.State = issuance.Reserved
			delivered.PairingID = strings.Repeat("a", 32)
			delivered.Label = "descendant"
			delivered.ExpiresAt = source.now().Add(time.Hour)
			must(t, issuance.Save(receiverHome, delivered))
			delivered.State = issuance.DeliveryIntent
			must(t, issuance.Save(receiverHome, delivered))
			delivered.State = issuance.Delivered
			must(t, issuance.Save(receiverHome, delivered))
			record, err := machineRecord(t.Context(), cfg, receiver)
			must(t, err)
			if len(record.UnusedSpares) != 1 || record.UnusedSpares[0].AccessKeyID != spare.ProviderID {
				t.Fatal("delivered spare represented as unused")
			}
			must(t, machines.Publish(t.Context(), store, record))
			verification := run(source, "", "machines", "--verify", "--yes", "--json")
			if !strings.Contains(verification, spare.ProviderID) || !strings.Contains(verification, "unused_spare_claim") {
				t.Fatal("remote spare omitted from provider verification")
			}
			proof := operatorBinding{Name: "portable", MachineID: cfg.MachineID, Assignment: *cfg.MachineAssignment, IndependentlyVerified: true, RetiredAssignments: cfg.RetiredMachineAssignments, UnusedSpares: []config.MachineAssignment{{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Own, AccessKeyID: spare.ProviderID, RecipientID: spare.RecipientID, IssuerID: spare.IssuerID, SlotID: spare.SlotID}}}
			binding := filepath.Join(t.TempDir(), "trusted-binding.json")
			must(t, local.Write(binding, proof))
			thirdHome := t.TempDir()
			third := setupTestEnv(t, thirdHome, t.TempDir(), newFakeKeychain(), source.now())
			third.Cloudflare = source.Cloudflare
			third.LookupEnv = source.LookupEnv
			third.UnsetEnv = receiver.UnsetEnv
			third.OpenStore = source.OpenStore
			thirdCfg := cfg
			thirdCfg.MachineID = strings.Repeat("f", 32)
			thirdCfg.MachineName = "operator"
			thirdCfg.MachineAssignment = nil
			thirdCfg.RetiredMachineAssignments = nil
			must(t, config.Save(thirdHome, thirdCfg))
			// A forged name cannot be promoted to authority by the operator file or --yes.
			forged := record
			forged.Name = "forged"
			must(t, machines.Publish(t.Context(), store, forged))
			var rejected bytes.Buffer
			if code := Run([]string{"machines", "revoke", "forged", "--binding-file", binding, "--yes"}, nil, &rejected, &rejected, third); code != 1 || !providerKeyLive(cf, cfg.MachineAssignment.AccessKeyID) {
				t.Fatal("bucket name redirected independent binding")
			}
			args := []string{"machines", "revoke", "--machine-id", cfg.MachineID, "--binding-file", binding, "--yes"}
			if mode == "issuer-compromise" {
				// Healthy operator evidence and provider inventory survive an erased compromised ledger.
				must(t, os.RemoveAll(filepath.Join(receiverHome, "issued")))
				args = append(args, "--include-issued")
			}
			if mode == "partial-retry" {
				cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: 403, Message: "synthetic interruption", Times: 1})
				var out bytes.Buffer
				if code := Run(args, nil, &out, &out, third); code != 1 {
					t.Fatal("partial deletion reported success")
				}
				progress, err := revocation.List(thirdHome)
				must(t, err)
				if len(progress) != 1 || !strings.Contains(progress[0].Summary(), "partial") {
					t.Fatal("partial results not durably retained")
				}
				run(third, "", "machines", "revoke", "--operation-id", progress[0].OperationID, "--yes")
			} else {
				run(third, "", args...)
			}
			if providerKeyLive(cf, cfg.MachineAssignment.AccessKeyID) || providerKeyLive(cf, spare.ProviderID) {
				t.Fatal("target key or unused spare retained")
			}
			if mode == "re-pair-retired" && providerKeyLive(cf, original.AccessKeyID) {
				t.Fatal("retired delivered assignment survived ordinary revocation")
			}
			if providerKeyLive(cf, delivered.ProviderID) != (mode != "issuer-compromise") {
				t.Fatal("delivered descendant ownership selection wrong")
			}
			sourceCfg, _, err := config.Load(sourceHome)
			must(t, err)
			if !providerKeyLive(cf, sourceCfg.MachineAssignment.AccessKeyID) {
				t.Fatal("source own key revoked")
			}
			listed := machines.List(t.Context(), store)
			if len(listed.Revocations) != 1 || listed.ProviderVerified {
				t.Fatal("progress missing or elevated to provider proof")
			}
		})
	}
}

func TestPairingStandaloneAndUnmappableExclusions(t *testing.T) {
	t.Parallel()
	sourceHome, destHome := t.TempDir(), t.TempDir()
	root := filepath.Join(sourceHome, "public")
	private := filepath.Join(sourceHome, "private")
	outside := t.TempDir()
	must(t, os.MkdirAll(root, 0700))
	must(t, os.MkdirAll(filepath.Join(destHome, "public"), 0700))
	env := Env{LookupEnv: noEnv, PairingRepoRoot: func(context.Context, string) (string, error) { return "", errors.New("no repo") }, repoKeyContext: func(context.Context, string) string { return "" }, WorkingDir: func() (string, error) { return "", nil }}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}, {Root: private, Included: false}, {Root: outside, Included: false}}}}
	inc, exc, err := exportPairingScope(t.Context(), cfg, sourceHome, env)
	must(t, err)
	if len(exc) != 2 || !exc[0].HomeRelative || exc[0].Path != "private" || !exc[1].Unresolved || strings.Contains(exc[1].Path, outside) {
		t.Fatal("standalone exclusion omitted or source path disclosed")
	}
	var output bytes.Buffer
	scopes, err := pairScope(newPrompter(strings.NewReader(""), &output), pairing.Payload{Inclusions: inc, Exclusions: exc}, config.Config{}, destHome, env, true)
	must(t, err)
	for _, scope := range scopes {
		if scope.Included {
			t.Fatal("unresolved restriction broadened noninteractive capture")
		}
	}
	if !slicesContainsExcluded(scopes, local.CanonicalPath(filepath.Join(destHome, "private"))) || !strings.Contains(output.String(), "unresolved") {
		t.Fatal("portable standalone exclusion not retained or warning omitted")
	}
	selected := map[string][]string{inc[0].ID: {filepath.Join(destHome, "public")}}
	mapped, withheld, err := mapPairingExclusions(newPrompter(strings.NewReader("public/private\n"), &output), pairing.Payload{Exclusions: exc[1:]}, selected, destHome, false)
	must(t, err)
	if len(mapped) != 1 || len(withheld) != 0 || mapped[0].Included {
		t.Fatal("interactive exclusion mapping failed")
	}
	for _, unsafe := range []string{"../escape", "/absolute"} {
		if _, _, err := mapPairingExclusions(newPrompter(strings.NewReader(unsafe+"\n"), &output), pairing.Payload{Exclusions: exc[1:]}, selected, destHome, false); err == nil {
			t.Fatal("unsafe mapping accepted")
		}
	}
}

func TestPairingClipboardUnavailableDeliberateFallback(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"print", "retry", "cancel"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			env, home, _ := pairingSourceFixture(t)
			var output bytes.Buffer
			payload, err := sourcePairingPayload(mustLoadPairConfig(t, home), "receiver", t.TempDir(), 15*time.Minute, env)
			must(t, err)
			ledger := pairingLedger{Version: 1, PairingID: payload.PairingID, RecipientID: payload.RecipientID, IssuerID: payload.IssuerID, Name: payload.Name, DestinationID: mustLoadPairConfig(t, home).DestinationID(), Kind: pairingAWSProfile, State: pairingPrepared, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt}
			calls := 0
			env.Clipboard = func([]byte) error {
				calls++
				if choice == "retry" && calls > 1 {
					return nil
				}
				return errors.New("clipboard unavailable")
			}
			p := newPrompter(strings.NewReader(choice+"\n"), &output)
			code := deliverPairingBundle(home, "aa-pair1:SYNTHETIC", &ledger, &issuance.Slot{}, nil, env, &output, &output, pairingAddOptions{prompt: p})
			if choice == "cancel" {
				if code != 1 || ledger.State != pairingDeliveryIntent {
					t.Fatal("uncertain delivery released")
				}
				return
			}
			if code != 0 || ledger.State != pairingDelivered {
				t.Fatalf("fallback: %d %s", code, &output)
			}
			if choice == "print" && !strings.Contains(output.String(), "aa-pair1:SYNTHETIC") {
				t.Fatal("encrypted bundle not delivered")
			}
			if choice == "print" && strings.Contains(output.String(), "bundle copied") {
				t.Fatal("file/print claimed clipboard delivery")
			}
		})
	}
}

func mustLoadPairConfig(t *testing.T, home string) config.Config {
	t.Helper()
	cfg, _, err := config.Load(home)
	must(t, err)
	return cfg
}

func TestManagementTokenFailureOffersDeliberateRetryAndPaste(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"retry", "paste", "cancel"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			calls := 0
			env := Env{LookupEnv: noEnv, Environ: func() []string { return nil }, RunTokenCommand: func(context.Context, []string, []string) (string, error) {
				calls++
				if calls == 1 {
					return "LEAK-CANARY", errors.New("LEAK-CANARY")
				}
				return "SYNTHETIC-TOKEN", nil
			}}
			var output bytes.Buffer
			p := newPrompter(strings.NewReader(choice+"\nSYNTHETIC-TOKEN\n"), &output)
			token, _, _, err := readManagementToken(t.Context(), p, env, []string{"synthetic"}, true)
			if strings.Contains(output.String(), "LEAK-CANARY") || strings.Contains(output.String(), "SYNTHETIC-TOKEN") {
				t.Fatal("secret output leaked")
			}
			if choice == "cancel" {
				if err == nil || token != "" {
					t.Fatal("cancel acquired token")
				}
			} else if err != nil || token != "SYNTHETIC-TOKEN" {
				t.Fatalf("fallback: %v %s", err, &output)
			}
			expectedCalls := 1
			if choice == "retry" {
				expectedCalls = 2
			}
			if calls != expectedCalls {
				t.Fatal("unrequested password manager retry")
			}
		})
	}
}

func TestCommittedPairingHousekeepingFailureRemainsSuccess(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	must(t, os.MkdirAll(draftPath(home), 0700))
	must(t, os.WriteFile(filepath.Join(draftPath(home), "prevent-remove"), []byte("synthetic"), 0600))
	cfg := config.Config{MachineID: strings.Repeat("a", 32)}
	must(t, config.Save(home, cfg))
	var output bytes.Buffer
	if err := finishSetup(newPrompter(strings.NewReader(""), &output), &output, home, cfg, false, nil, env.now(), setupFinish{env: env, userHome: userHome}); err != nil || !strings.Contains(output.String(), "committed") || !strings.Contains(output.String(), "Configuration saved") {
		t.Fatalf("postcommit cleanup misreported %v %s", err, &output)
	}
}

func TestRetiredHistoryIgnoresDraftAndKeepsDestinationBindings(t *testing.T) {
	t.Parallel()
	a := config.MachineAssignment{DestinationID: strings.Repeat("a", 64), Kind: config.MachineAssignmentR2Own, AccessKeyID: strings.Repeat("a", 32), RecipientID: strings.Repeat("b", 32), IssuerID: strings.Repeat("c", 32), SlotID: strings.Repeat("d", 32)}
	old := config.Config{RetiredMachineAssignments: []config.MachineAssignment{a}}
	next := config.Config{RetiredMachineAssignments: []config.MachineAssignment{{AccessKeyID: "forged-draft"}}}
	mergeCommittedSetupState(old, &next, nil)
	if len(next.RetiredMachineAssignments) != 1 || next.RetiredMachineAssignments[0].AccessKeyID != a.AccessKeyID {
		t.Fatal("draft replaced committed history")
	}
	expected := map[string]revocation.Key{}
	addAssignmentKey(expected, a, strings.Repeat("e", 64))
	if len(expected) != 0 {
		t.Fatal("different destination selected")
	}
	addAssignmentKey(expected, a, a.DestinationID)
	if len(expected) != 1 {
		t.Fatal("retired binding missing")
	}
}

func TestDirectDiscoveryAvoidsMalformedHistoryAndPartialReviewIsExplicit(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	must(t, os.MkdirAll(root, 0700))
	must(t, os.MkdirAll(filepath.Join(home, ".claude", "projects", "bad"), 0700))
	must(t, os.WriteFile(filepath.Join(home, ".claude", "projects", "bad", "bad.jsonl"), []byte("broken header"), 0600))
	env := Env{LookupEnv: noEnv, WorkingDir: func() (string, error) { return root, nil }, repoKeyContext: func(context.Context, string) string { return "repo-0123456789abcdef" }}
	match := matchProjects(t.Context(), env, home, config.Config{}, []projectMatchRequest{{RepoKey: "repo-0123456789abcdef"}})
	if match.Incomplete || len(match.Roots[0]) != 1 || match.History.Unreadable != 0 {
		t.Fatal("direct match consumed unnecessary history")
	}
	payload := pairing.Payload{Inclusions: []pairing.Inclusion{{ID: strings.Repeat("a", 32), Label: "repo", RepoKey: "repo-0123456789abcdef"}}}
	match.Incomplete = true
	var output bytes.Buffer
	selected, err := choosePairingScopes(newPrompter(strings.NewReader("yes\n"), &output), payload, match, home, false, env)
	must(t, err)
	if len(selected) != 1 || !strings.Contains(output.String(), "other clones may exist") {
		t.Fatal("partial evidence discarded or implied unique")
	}
}

func TestRevocationProgressUsesLocalResultsAndBucketClaimsSeparately(t *testing.T) {
	t.Parallel()
	env, home, _, cfg, store := revocationFixture(t)
	j, err := prepareRevocation(home, cfg, revokeSelector{MachineID: cfg.MachineID}, "", env)
	must(t, err)
	j.RequestOnly = true
	must(t, revocation.Save(home, j))
	must(t, putRevocation(t.Context(), store, j))
	if !strings.Contains(strings.Join(pairingWarnings(home, env.now()), " "), "Local revocation") {
		t.Fatal("status omitted local operation")
	}
	listing := machines.List(t.Context(), store)
	if len(listing.Revocations) != 1 || len(listing.Records) != 1 || listing.ProviderVerified {
		t.Fatal("bucket progress promoted to identity/authority")
	}
	raw, _ := json.Marshal(listing)
	if !bytes.Contains(raw, []byte("request_only")) {
		t.Fatal("JSON omitted request distinction")
	}
	key := revocation.Key{Outcome: revocation.Unknown}
	j.RequestOnly = false
	j.Keys = []revocation.Key{key}
	if !strings.Contains(j.Summary(), "unknown") {
		t.Fatal("unknown became confirmed")
	}
	j.Keys = append(j.Keys, revocation.Key{Outcome: revocation.Confirmed})
	if !strings.Contains(j.Summary(), "partial") {
		t.Fatal("partial became confirmed")
	}
	j.Keys[0].Outcome = revocation.Confirmed
	if !strings.Contains(j.Summary(), "provider-confirmed") {
		t.Fatal("confirmed hidden")
	}
}

func TestRetiredOnlyLocalRevocationAndRecordHistoryBounds(t *testing.T) {
	env, home, cf, cfg, _ := revocationFixture(t)
	key := cfg.MachineAssignment.AccessKeyID
	cfg.RetiredMachineAssignments = []config.MachineAssignment{*cfg.MachineAssignment}
	cfg.MachineAssignment = nil
	must(t, config.Save(home, cfg))
	var out bytes.Buffer
	if code := Run([]string{"machines", "revoke", "--machine-id", cfg.MachineID, "--yes"}, nil, &out, &out, env); code != 0 || providerKeyLive(cf, key) {
		t.Fatalf("retired-only binding not revoked: %d %s", code, &out)
	}
	for i := 1; i < 18; i++ {
		a := cfg.RetiredMachineAssignments[0]
		a.AccessKeyID = strings.Repeat("e", 31) + string("0123456789abcdef"[i%16])
		a.SlotID = a.AccessKeyID
		cfg.RetiredMachineAssignments = append(cfg.RetiredMachineAssignments, a)
	}
	// Use unique bounded IDs for large history without risking registry size growth.
	for i := range cfg.RetiredMachineAssignments {
		cfg.RetiredMachineAssignments[i].AccessKeyID = fmt.Sprintf("%032x", i+1)
	}
	r, err := machines.Build(cfg, "darwin/arm64", "dev", "", env.now())
	must(t, err)
	if len(r.RetiredCredentials) != 16 || !r.CredentialHistoryPartial {
		t.Fatal("large history silently omitted or record unbounded")
	}
	store := storagetest.NewMemoryStore()
	must(t, machines.Publish(t.Context(), store, r))
}

func TestPairingMissingAppRecheckAndManualRepositoryMismatch(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	calls := 0
	env.DetectHarnesses = func(string) []string {
		calls++
		if calls == 1 {
			return nil
		}
		return []string{"codex"}
	}
	var out bytes.Buffer
	cfg, err := pairingCaptureSettings(newPrompter(strings.NewReader("retry\n"), &out), pairing.Payload{Apps: []string{"codex"}, SkillEvidence: "metadata"}, config.Config{}, config.Config{}, userHome, setupOptions{projects: []string{userHome}, codexCaptureScope: string(config.CodexIncludedProjects)}, env)
	must(t, err)
	if len(cfg.Harnesses) != 1 || calls != 2 || !strings.Contains(out.String(), "not found") {
		t.Fatal("missing app was not recovered")
	}
	root := filepath.Join(userHome, "wrong")
	must(t, os.MkdirAll(root, 0700))
	env.repoKeyContext = func(context.Context, string) string { return "repo-fedcba9876543210" }
	payload := pairing.Payload{Inclusions: []pairing.Inclusion{{ID: strings.Repeat("a", 32), RepoKey: "repo-0123456789abcdef", Label: "repo"}}}
	_, err = choosePairingScopes(newPrompter(strings.NewReader(root+"\n"), &out), payload, projectMatchResult{Roots: make([][]string, 1)}, userHome, false, env)
	if err == nil {
		t.Fatal("manual mismatched repository broadened scope")
	}
	payload.Inclusions[0].RepoKey = ""
	env.repoKeyContext = func(context.Context, string) string { return "invalid-repository-evidence" }
	selected, err := choosePairingScopes(newPrompter(strings.NewReader(root+"\n"), &out), payload, projectMatchResult{Roots: make([][]string, 1)}, userHome, false, env)
	must(t, err)
	if len(selected) != 1 || !strings.Contains(out.String(), "explicit path") {
		t.Fatal("explicit manual non-repository recovery failed")
	}
}

func TestRetiredHistoryRollsBackWithFailedReplacement(t *testing.T) {
	t.Parallel()
	env, home, _, cfg, _ := revocationFixture(t)
	original := cfg.MachineAssignment.AccessKeyID
	next := cfg
	assignment := *cfg.MachineAssignment
	assignment.AccessKeyID = strings.Repeat("d", 32)
	assignment.SlotID = strings.Repeat("e", 32)
	next.MachineAssignment = &assignment
	next.Storage.R2CredentialRef = "replacement"
	fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic interrupted setup") }
	userHome, err := env.userHomeDir()
	must(t, err)
	exe, err := env.executable()
	must(t, err)
	if err = applySetup(home, userHome, exe, cfg, &next, nil, env); err == nil {
		t.Fatal("failed replacement reported committed")
	}
	got, _, err := config.Load(home)
	must(t, err)
	if len(got.RetiredMachineAssignments) != 0 || got.MachineAssignment.AccessKeyID != original {
		t.Fatal("failed transaction changed committed retirement history")
	}
}

func TestPairingExclusionRecoveryRetainsMappedClones(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("interactive=%t", interactive), func(t *testing.T) {
			t.Parallel()
			home := local.CanonicalPath(t.TempDir())
			outside := t.TempDir()
			roots := []string{filepath.Join(home, "one"), filepath.Join(home, "two"), filepath.Join(home, "three")}
			for _, root := range roots {
				must(t, os.MkdirAll(root, 0700))
			}
			must(t, os.MkdirAll(filepath.Join(roots[0], "private"), 0700))
			for _, root := range roots[1:] {
				must(t, os.Symlink(outside, filepath.Join(root, "private")))
			}
			must(t, os.MkdirAll(filepath.Join(roots[1], "recovered"), 0700))
			id := strings.Repeat("a", 32)
			payload := pairing.Payload{Inclusions: []pairing.Inclusion{{ID: id, Label: "repo"}}, Exclusions: []pairing.Exclusion{{InclusionID: id, Path: "private", Affected: []string{id}}}}
			selected := map[string][]string{id: roots}
			var output bytes.Buffer
			input := ""
			if interactive {
				input = "two/recovered\n"
			}
			exclusions, withheld, err := mapPairingExclusions(newPrompter(strings.NewReader(input), &output), payload, selected, home, !interactive)
			must(t, err)
			if !slicesContainsExcluded(exclusions, filepath.Join(roots[0], "private")) {
				t.Fatal("successful first-clone exclusion was lost")
			}
			wantedClones := 1
			if interactive {
				wantedClones = 2
			}
			if withheld[id] || len(selected[id]) != wantedClones {
				t.Fatal("safe clone lost or unresolved clone enabled")
			}
			for _, root := range selected[id] {
				if root == roots[2] {
					t.Fatal("unmapped third clone remained capturable")
				}
			}
			if interactive && !slicesContainsExcluded(exclusions, filepath.Join(roots[1], "recovered")) {
				t.Fatal("manual recovery mapping was lost")
			}
		})
	}
}

func TestManualPairingScopeExpandsReceivingHome(t *testing.T) {
	t.Parallel()
	home := local.CanonicalPath(t.TempDir())
	root := filepath.Join(home, "repo")
	must(t, os.MkdirAll(root, 0700))
	env := Env{repoKeyContext: func(context.Context, string) string { return "repo-0123456789abcdef" }}
	for _, input := range []string{"~/repo", "repo", root} {
		var output bytes.Buffer
		mapped, key, known, err := manualPairingScope(newPrompter(strings.NewReader(input+"\n"), &output), pairing.Inclusion{Label: "repo", RepoKey: "repo-0123456789abcdef"}, home, env)
		must(t, err)
		if mapped != root || !known || key != "repo-0123456789abcdef" {
			t.Fatalf("manual recovery resolved %q to %q", input, mapped)
		}
	}
	var output bytes.Buffer
	mapped, _, _, err := manualPairingScope(newPrompter(strings.NewReader("~\n"), &output), pairing.Inclusion{Label: "home"}, home, env)
	must(t, err)
	if mapped != home {
		t.Fatal("bare tilde did not resolve to receiving home")
	}
}

// A deleted token may no longer be visible, regardless of inventory completeness.
type absentRetiredTokenAPI struct {
	cloudflare.API
	cloudflare.InventoryAPI
	retiredID string
}

func (a absentRetiredTokenAPI) TokenDetails(ctx context.Context, account, id string) (cloudflare.TokenMetadata, error) {
	if id == a.retiredID {
		return cloudflare.TokenMetadata{}, errors.New("deleted token not visible")
	}
	return a.InventoryAPI.TokenDetails(ctx, account, id)
}

func TestRetiredConfirmationRequiresExactLocalEvidence(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*revocation.Journal){
		"exact":        func(*revocation.Journal) {},
		"destination":  func(j *revocation.Journal) { j.DestinationID = strings.Repeat("d", 64) },
		"account":      func(j *revocation.Journal) { j.AccountID = strings.Repeat("d", 32) },
		"bucket":       func(j *revocation.Journal) { j.Bucket = "other-bucket" },
		"jurisdiction": func(j *revocation.Journal) { j.Jurisdiction = "eu" },
		"permission":   func(j *revocation.Journal) { j.PermissionID = strings.Repeat("d", 32) },
		"requester":    func(j *revocation.Journal) { j.RequesterID = strings.Repeat("d", 32) },
		"provider":     func(j *revocation.Journal) { j.Keys[0].ProviderID = strings.Repeat("d", 32) },
		"recipient":    func(j *revocation.Journal) { j.Keys[0].RecipientID = strings.Repeat("d", 32) },
		"issuer":       func(j *revocation.Journal) { j.Keys[0].IssuerID = strings.Repeat("d", 32) },
		"slot":         func(j *revocation.Journal) { j.Keys[0].SlotID = strings.Repeat("d", 32) },
		"unknown":      func(j *revocation.Journal) { j.Keys[0].Outcome = revocation.Unknown },
		"pending":      func(j *revocation.Journal) { j.Keys[0].Outcome = revocation.Pending },
		"bucket-claim": func(*revocation.Journal) {},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, home, _, cfg, store := revocationFixture(t)
			retired := *cfg.MachineAssignment
			current := retired
			current.AccessKeyID = strings.Repeat("e", 32)
			current.SlotID = strings.Repeat("f", 32)
			cfg.MachineAssignment = &current
			cfg.RetiredMachineAssignments = []config.MachineAssignment{retired}
			planned, err := prepareRevocation(home, cfg, revokeSelector{MachineID: cfg.MachineID}, "", env)
			must(t, err)
			planned.PermissionID = strings.Repeat("a", 32)
			prior := planned
			prior.OperationID, err = local.ID()
			must(t, err)
			prior.Keys = []revocation.Key{{ProviderID: retired.AccessKeyID, RecipientID: retired.RecipientID, IssuerID: retired.IssuerID, SlotID: retired.SlotID, Outcome: revocation.Confirmed}, {ProviderID: strings.Repeat("e", 32), RecipientID: current.RecipientID, IssuerID: current.IssuerID, SlotID: current.SlotID, Outcome: revocation.Unknown}}
			change(&prior)
			if name == "bucket-claim" {
				must(t, putRevocation(t.Context(), store, prior))
			} else {
				must(t, revocation.Save(home, prior))
			}
			expected, err := committedRevocationKeys(cfg, cfg.MachineID, &current, nil)
			must(t, err)
			must(t, excludeConfirmedRetiredKeys(home, cfg, planned, cfg.MachineID, &current, nil, expected))
			_, retained := expected[retired.AccessKeyID]
			if retained != (name != "exact") {
				t.Fatal("confirmation crossed a trust, scope, binding or outcome boundary")
			}
			if _, present := expected[current.AccessKeyID]; !present {
				t.Fatal("confirmation omitted current assignment")
			}
			if len(cfg.RetiredMachineAssignments) != 1 {
				t.Fatal("confirmation erased retained history")
			}
		})
	}
}

func TestPairingHomeExclusionStillRequiresWholeScopeCoverage(t *testing.T) {
	t.Parallel()
	home := local.CanonicalPath(t.TempDir())
	root := filepath.Join(home, "repo")
	must(t, os.MkdirAll(filepath.Join(root, "private"), 0700))
	id := strings.Repeat("a", 32)
	selected := map[string][]string{id: {root}}
	payload := pairing.Payload{Exclusions: []pairing.Exclusion{{HomeRelative: true, Path: "repo/private", Affected: []string{id}}}}
	var out bytes.Buffer
	exclusions, withheld, err := mapPairingExclusions(newPrompter(strings.NewReader(""), &out), payload, selected, home, true)
	must(t, err)
	if !withheld[id] || len(selected[id]) != 0 || !slicesContainsExcluded(exclusions, filepath.Join(root, "private")) {
		t.Fatal("partial home-relative coverage broadened a restricted scope")
	}
}
