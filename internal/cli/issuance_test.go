package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func dedicatedFixture(t *testing.T) (Env, string, *cloudflaretest.Server, *fakeKeychain) {
	t.Helper()
	env, home, _ := pairingSourceFixture(t)
	cfg, _, err := config.Load(home)
	must(t, err)
	cfg.Storage = credentials.Config{Provider: credentials.ProviderR2, Bucket: "synthetic", Prefix: "archive/", R2AccountID: cloudflaretest.AccountID, R2CredentialRef: "main"}
	kc := newFakeKeychain()
	must(t, kc.Save(context.Background(), "main", credentials.R2Credentials{AccessKeyID: strings.Repeat("e", 32), SecretAccessKey: "object-canary"}))
	must(t, config.Save(home, cfg))
	env.Credentials = func() (credentials.CredentialStore, error) { return kc, nil }
	cf := cloudflaretest.New(t, bootstrapCanary)
	env.Cloudflare = func(token string) cloudflare.API {
		return cloudflare.New(token, cloudflare.Options{BaseURL: cf.URL + "/client/v4", Sleep: func(context.Context, time.Duration) error { return nil }})
	}
	env.Pause = func(time.Duration) {}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS" {
			return "1", true
		}
		return "", false
	}
	env.UnsetEnv = func(string) error { return nil }
	return env, home, cf, kc
}

func fixtureIssuer(t *testing.T, env Env, home string) *keyIssuer {
	t.Helper()
	cfg, _, err := config.Load(home)
	must(t, err)
	i, err := newKeyIssuer(home, cfg, env, newPrompter(strings.NewReader(""), &bytes.Buffer{}), env.cloudflareAPI(bootstrapCanary))
	must(t, err)
	t.Cleanup(i.api.Discard)
	return i
}

func TestDedicatedCreationReservationAndSecretFreeLedger(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	defer release()
	s, key, err := i.create(issuance.Precreated)
	must(t, err)
	identity, ok := cloudflare.ParseProviderName(cf.Tokens()[0].Name)
	if !ok || identity.SlotID != s.SlotID || identity.RecipientID != s.RecipientID || identity.IssuerID != i.cfg.MachineID {
		t.Fatal("immutable name mismatch")
	}
	selected, loaded, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	if selected.SlotID != s.SlotID || loaded != key {
		t.Fatal("wrong reserved secret")
	}
	second, _, err := reserveSpare(home, i.cfg, strings.Repeat("c", 32), "desktop", env.now().Add(time.Hour), env)
	must(t, err)
	if second.SlotID != "" {
		t.Fatal("reserved twice")
	}
	selected.State = issuance.DeliveryIntent
	must(t, issuance.Save(home, selected))
	releaseUntouchedSlot(home, &selected, nil)
	discardDeliveredSecret(home, &selected, env, &bytes.Buffer{})
	if _, err = kc.Load(context.Background(), s.SecretRef); err == nil {
		t.Fatal("delivered spare secret retained")
	}
	data, err := os.ReadFile(filepath.Join(home, "issued", "slot-"+s.SlotID+".json"))
	must(t, err)
	for _, secret := range []string{key.SecretAccessKey, bootstrapCanary, cf.Tokens()[0].Value} {
		if strings.Contains(string(data), secret) {
			t.Fatal("ledger leaked a secret")
		}
	}
	slots, err := issuance.List(home)
	must(t, err)
	if slots[0].State != issuance.DeliveryIntent {
		t.Fatal("uncertain exposure recycled")
	}
}

func TestDedicatedLostResponseReconcilesUniqueSlot(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	client := i.api
	i.api = &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(ctx context.Context, a string, s cloudflare.TokenSpec) (cloudflare.Token, error) {
		_, err := client.CreateToken(ctx, a, s)
		must(t, err)
		return cloudflare.Token{}, errors.New("lost response canary")
	}}
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.Deleted || len(cf.Live()) != 0 || len(cf.Tokens()) != 1 {
		t.Fatalf("lost creation not cleaned: state %s live %d", s.State, len(cf.Live()))
	}
	// Creation intent alone is enough to reconcile a crash without the one-time value.
	s, err = issuance.New(i.cfg.MachineID, i.cfg.DestinationID(), i.account, i.bucket, i.group, issuance.Fresh, env.now())
	must(t, err)
	must(t, issuance.Save(home, s))
	resource, err := cloudflare.BucketResource(i.account, i.bucket)
	must(t, err)
	_, err = client.CreateToken(context.Background(), i.account, cloudflare.TokenSpec{Name: s.ProviderName, Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{i.group}, Resources: map[string]string{resource: "*"}}}})
	must(t, err)
	must(t, i.reconcile())
	if len(cf.Live()) != 0 {
		t.Fatal("crash orphan survived reconciliation")
	}
}

type trackedInventoryAPI struct {
	cloudflare.API
	cloudflare.InventoryAPI
	create func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error)
}

func (a *trackedInventoryAPI) CreateToken(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
	return a.create(ctx, account, spec)
}

func TestDedicatedUnknownCleanupRemainsPending(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	client := i.api
	i.api = &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error) {
		return cloudflare.Token{}, errors.New("lost")
	}}
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.CleanupPending || len(cf.Tokens()) != 0 {
		t.Fatal("unknown creation treated as known absent")
	}
	slots, err := issuance.List(home)
	must(t, err)
	if slots[0].State != issuance.CleanupPending {
		t.Fatal("uncertainty lost")
	}
}

func TestDedicatedAddFreshThenSpareAndUncertainDelivery(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	env.LookupEnv = func(key string) (string, bool) {
		switch key {
		case "CLOUDFLARE_API_TOKEN":
			return bootstrapCanary, true
		case "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS":
			return "1", true
		}
		return "", false
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("fresh add %d %s", code, &errOut)
	}
	if len(cf.Tokens()) != 3 || len(kc.items) != 3 {
		t.Fatalf("fresh+two spares, secrets %d tokens %d", len(kc.items), len(cf.Tokens()))
	}
	lines := strings.Split(out.String(), "\n")
	var bundle, code string
	for _, line := range lines {
		if strings.HasPrefix(line, "aa-pair1:") {
			bundle = line
		}
		if strings.HasPrefix(line, "Pairing code (deliver separately): ") {
			code = strings.TrimPrefix(line, "Pairing code (deliver separately): ")
		}
	}
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if payload.Kind != config.MachineAssignmentR2Own || !pairing.ValidID(payload.SlotID) {
		t.Fatal("dedicated provenance absent")
	}
	env.LookupEnv = func(key string) (string, bool) { return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS" }
	out.Reset()
	errOut.Reset()
	path := filepath.Join(t.TempDir(), "existing")
	must(t, os.WriteFile(path, []byte("preserve"), 0600))
	if Run([]string{"machines", "add", "--yes", "--name", "desktop", "--file", path}, strings.NewReader(""), &out, &errOut, env) != 1 {
		t.Fatal("uncertain file delivery accepted")
	}
	if len(cf.Tokens()) != 3 || len(kc.items) != 2 {
		t.Fatal("spare reused or delivered secret retained")
	}
	slots, err := issuance.List(home)
	must(t, err)
	intents := 0
	for _, s := range slots {
		if s.State == issuance.DeliveryIntent {
			intents++
		}
	}
	if intents != 1 {
		t.Fatal("missing delivery intent")
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if len(cfg.SpareCredentialRefs) != 1 {
		t.Fatal("advisory index stale")
	}
}

func TestDedicatedNoImplicitSharingAndCorruptLedger(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &out, env) != 1 || !strings.Contains(out.String(), "no key was shared") || len(cf.Tokens()) != 0 {
		t.Fatal("implicit sharing")
	}
	must(t, os.MkdirAll(filepath.Join(home, "issued"), 0700))
	must(t, os.WriteFile(filepath.Join(home, "issued", "slot-"+strings.Repeat("d", 32)+".json"), []byte("{}"), 0600))
	out.Reset()
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &out, env) != 1 || !strings.Contains(out.String(), "spares withheld") {
		t.Fatal("corrupt ledger granted eligibility")
	}
}

func TestDedicatedIssuanceLockExcludesSecondReservation(t *testing.T) {
	env, home, _, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	first, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	_, _, err = i.create(issuance.Precreated)
	must(t, err)
	done := make(chan error, 1)
	go func() {
		unlock, err := local.NamedLock(home, "issued.lock")
		if err == nil {
			unlock()
		}
		done <- err
	}()
	if err = <-done; !errors.Is(err, local.ErrBusy) {
		t.Fatal("second process admitted")
	}
	first()
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	defer release()
	slot, _, err := reserveSpare(home, i.cfg, strings.Repeat("b", 32), "laptop", env.now().Add(time.Hour), env)
	must(t, err)
	if slot.SlotID == "" {
		t.Fatal("first reservation missing")
	}
	slot, _, err = reserveSpare(home, i.cfg, strings.Repeat("c", 32), "desktop", env.now().Add(time.Hour), env)
	must(t, err)
	if slot.SlotID != "" {
		t.Fatal("same spare handed out twice")
	}
}

func TestDedicatedVerificationFailureCleansBeforeExposure(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	i.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("storage refused") }
	s, _, err := i.create(issuance.Fresh)
	if err == nil || s.State != issuance.Deleted || len(cf.Live()) != 0 {
		t.Fatal("failed pre-exposure key left active")
	}
}

func TestDedicatedRefillFailureKeepsDeliveredBundleValid(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	original := env.Cloudflare
	calls := 0
	env.Cloudflare = func(token string) cloudflare.API {
		client := original(token)
		return &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(ctx context.Context, a string, s cloudflare.TokenSpec) (cloudflare.Token, error) {
			calls++
			if calls > 1 {
				return cloudflare.Token{}, &cloudflare.Error{Status: 403}
			}
			return client.CreateToken(ctx, a, s)
		}}
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS"
	}
	var out, errOut bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env) != 0 || !strings.Contains(errOut.String(), "Pairing remains valid") {
		t.Fatal("refill invalidated delivered pairing")
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if len(cf.Live()) != 1 || payload.AccessKeyID != cf.Live()[0].ID {
		t.Fatal("delivered key deleted after independent refill failure")
	}
	slots, err := issuance.List(home)
	must(t, err)
	delivered := 0
	for _, s := range slots {
		if s.State == issuance.Delivered {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatal("missing delivered lineage")
	}
}

func pairingPieces(output string) (bundle, code string) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "aa-pair1:") {
			bundle = line
		}
		if strings.HasPrefix(line, "Pairing code (deliver separately): ") {
			code = strings.TrimPrefix(line, "Pairing code (deliver separately): ")
		}
	}
	return bundle, code
}

func TestDedicatedFreshRefusalFallsBackToExistingSpare(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	i := fixtureIssuer(t, env, home)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	s, _, err := i.create(issuance.Precreated)
	must(t, err)
	release()
	original := env.Cloudflare
	env.Cloudflare = func(token string) cloudflare.API {
		client := original(token)
		return &trackedInventoryAPI{API: client, InventoryAPI: client.(cloudflare.InventoryAPI), create: func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error) {
			return cloudflare.Token{}, &cloudflare.Error{Status: 403}
		}}
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS"
	}
	var out, errOut bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop"}, strings.NewReader(""), &out, &errOut, env) != 0 {
		t.Fatalf("spare fallback failed: %s", &errOut)
	}
	bundle, code := pairingPieces(out.String())
	payload, err := pairing.Open(bundle, code, env.now())
	must(t, err)
	if payload.SlotID != s.SlotID || len(cf.Tokens()) != 1 || !strings.Contains(out.String(), "eligible spare instead") {
		t.Fatal("fresh refusal lost valid spare")
	}
}

func TestDedicatedSparesZeroAndFlagBounds(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	var out bytes.Buffer
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--spares=-1"}, strings.NewReader(""), &out, &out, env) != 2 {
		t.Fatal("negative spare flag accepted")
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return bootstrapCanary, true
		}
		return "1", key == "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_KEYS"
	}
	out.Reset()
	if Run([]string{"machines", "add", "--yes", "--name", "laptop", "--spares=0"}, strings.NewReader(""), &out, &out, env) != 0 {
		t.Fatal("zero spare issuance failed")
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	if cfg.SpareTarget() != 0 || len(cf.Tokens()) != 1 || len(cfg.SpareCredentialRefs) != 0 {
		t.Fatal("zero policy did not disable refill")
	}
}
