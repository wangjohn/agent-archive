package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type registrationStore struct {
	*storagetest.MemoryStore
	fail bool
	puts int
}

func (s *registrationStore) Put(ctx context.Context, key string, b []byte) error {
	if strings.HasPrefix(key, "machines/") {
		s.puts++
		if s.fail {
			return errors.New("secret provider details")
		}
	}
	return s.MemoryStore.Put(ctx, key, b)
}

func TestMachinePublicationRetriesAndRepairsDailyWithoutBlockingCapture(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Now()
	env := testEnv(t, home, now)
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "laptop", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}, Archive: archive.Config{Enabled: true}}
	s := &registrationStore{MemoryStore: storagetest.NewMemoryStore(), fail: true}
	if e := config.Save(home, cfg); e != nil {
		t.Fatal(e)
	}
	if e := publishMachineLocked(context.Background(), home, cfg, env, s, false); e == nil {
		t.Fatal("expected failure")
	}
	kept, found, e := config.Load(home)
	if e != nil || !found || !kept.Archive.Enabled || !registrationPending(home, cfg) {
		t.Fatal("publication changed capture")
	}
	s.fail = false
	if e := publishMachineLocked(context.Background(), home, cfg, env, s, false); e != nil {
		t.Fatal(e)
	}
	if registrationPending(home, cfg) {
		t.Fatal("pending after successful retry")
	}
	s.puts = 0
	if e := publishMachineLocked(context.Background(), home, cfg, env, s, false); e != nil || s.puts != 0 {
		t.Fatal("duplicate daily heartbeat")
	}
	_ = s.Delete(context.Background(), "machines/"+cfg.MachineID+".json")
	env.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if e := publishMachineLocked(context.Background(), home, cfg, env, s, false); e != nil || s.puts != 1 {
		t.Fatal("daily missing record not recreated")
	}
	cfg.Storage.Bucket = "other"
	if e := publishMachineLocked(context.Background(), home, cfg, env, s, false); e != nil || s.puts != 2 {
		t.Fatal("destination ack reused")
	}
	var ack machineRegistration
	if e := local.Read(filepath.Join(home, machineRegistrationFile), &ack); e != nil || ack.DestinationID != cfg.DestinationID() {
		t.Fatal("ack destination mismatch")
	}
}

func TestSetupPublicationFailureKeepsCommittedCapture(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	s := &registrationStore{MemoryStore: storagetest.NewMemoryStore(), fail: true}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	out := setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project)
	cfg, found, e := config.Load(home)
	if e != nil || !found || !cfg.Archive.Enabled || !strings.Contains(out, "Machine registration pending") || strings.Contains(out, "secret provider") {
		t.Fatalf("%v %v %s", found, e, out)
	}
	s.fail = false
	if e := publishMachineAfterSetup(home, env); e != nil {
		t.Fatal(e)
	}
	got := machines.List(context.Background(), s)
	if len(got.Records) != 1 || got.Records[0].MachineID != cfg.MachineID {
		t.Fatalf("%+v", got)
	}
}

func TestMachinesRenameKeepsIdentityAndRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "old", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}, Archive: archive.Config{Enabled: true}}
	must(t, config.Save(home, cfg))
	r, e := machines.Build(cfg, "linux/amd64", "dev", "", time.Now())
	must(t, e)
	must(t, machines.Publish(context.Background(), s, r))
	other := r
	other.MachineID = strings.Repeat("b", 32)
	other.Name = "taken"
	must(t, machines.Publish(context.Background(), s, other))
	var out bytes.Buffer
	if code := Run([]string{"machines", "rename", "taken"}, nil, &out, &out, env); code != 1 {
		t.Fatalf("duplicate accepted: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"machines", "rename", cfg.MachineID, "new"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("%d %s", code, out.String())
	}
	after, _, e := config.Load(home)
	must(t, e)
	if after.MachineID != cfg.MachineID || after.MachineName != "new" || after.Storage != cfg.Storage {
		t.Fatalf("%+v", after)
	}
	out.Reset()
	if code := Run([]string{"machines", "--json"}, nil, &out, &out, env); code != 0 || strings.Contains(out.String(), "secret") {
		t.Fatalf("%d %s", code, out.String())
	}
}

func TestCollectorRecreatesMachineRecordWithoutSetupDraft(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project)
	cfg, _, e := config.Load(home)
	must(t, e)
	must(t, s.Delete(context.Background(), "machines/"+cfg.MachineID+".json"))
	env.Now = func() time.Time { return now.Add(24 * time.Hour) }
	_, e = runOnePass(env, false)
	must(t, e)
	got := machines.List(context.Background(), s)
	if len(got.Records) != 1 || got.Records[0].MachineID != cfg.MachineID {
		t.Fatalf("%+v", got)
	}
}

// This changes the process-wide release version, so it runs before parallel tests.
func TestRefreshLeavesRegistrationUntouchedUntilCollectorUpdatesVersion(t *testing.T) {
	old := Version
	Version = "registry-old"
	t.Cleanup(func() { Version = old })
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	s := storagetest.NewMemoryStore()
	opens := 0
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { opens++; return s, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project)
	var before, after machineRegistration
	must(t, local.Read(filepath.Join(home, machineRegistrationFile), &before))
	Version = "registry-new"
	opens = 0
	code, out, errOut := refreshRun(t, env)
	if code != 0 || opens != 0 {
		t.Fatalf("%d opens=%d %s %s", code, opens, out, errOut)
	}
	must(t, local.Read(filepath.Join(home, machineRegistrationFile), &after))
	if before != after {
		t.Fatal("refresh changed registration ack")
	}
	_, e := runOnePass(env, false)
	must(t, e)
	result := machines.List(context.Background(), s)
	if len(result.Records) != 1 || result.Records[0].AgentArchiveVersion != "registry-new" {
		t.Fatalf("%+v", result)
	}
}

func TestRenamePreservesCommittedCredentialAssignment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "before", Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}, Archive: archive.Config{Enabled: true}}
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: config.MachineAssignmentR2Shared, AccessKeyID: "synthetic-key", RecipientID: strings.Repeat("b", 32), IssuerID: strings.Repeat("c", 32), SharedWith: strings.Repeat("c", 32)}
	must(t, config.Save(home, cfg))
	record, e := machines.Build(cfg, "linux/amd64", "dev", "", time.Now())
	must(t, e)
	must(t, machines.Publish(context.Background(), s, record))
	var out bytes.Buffer
	if code := Run([]string{"machines", "rename", "after"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("%d %s", code, out.String())
	}
	got, _, e := config.Load(home)
	must(t, e)
	if got.MachineAssignment == nil || *got.MachineAssignment != *cfg.MachineAssignment || got.MachineID != cfg.MachineID {
		t.Fatal("rename changed immutable credential identity")
	}
	if code := Run([]string{"machines", "--json"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("list exit %d", code)
	}
}

func TestMachineRecordsStayOutsideSessionListingAndRetention(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project)
	var out bytes.Buffer
	if code := Run([]string{"list", "--json", "--all-projects", "--limit", "0"}, nil, &out, &out, env); code != 0 || strings.Contains(out.String(), "unnamed-") {
		t.Fatalf("session list included machine: %d %s", code, out.String())
	}
	_, e := runOnePass(env, false)
	must(t, e)
	if got := machines.List(context.Background(), s); len(got.Records) != 1 {
		t.Fatalf("retention removed registry: %+v", got)
	}
}

func TestFirstSetupNamesMachineAfterBoundedDuplicateObservation(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	other, err := machines.Build(config.Config{MachineID: strings.Repeat("b", 32), MachineName: "taken", Storage: credentials.Config{Provider: credentials.ProviderS3}}, "linux/amd64", "dev", "", time.Now())
	must(t, err)
	must(t, machines.Publish(context.Background(), s, other))
	input := strings.TrimSuffix(s3SetupInput("b", "us-east-1", "p", true, false, false, project), "y\n") + "machine\nbad name\ntaken\nwork-laptop\ny\n"
	out := setupRun(t, env, input, 0)
	cfg, _, err := config.Load(home)
	must(t, err)
	if cfg.MachineName != "work-laptop" || !strings.Contains(out, "name is already used") || setupReceiptIndex(out, "Machine work-laptop") < 0 {
		t.Fatalf("name=%q\n%s", cfg.MachineName, out)
	}
	got := machines.List(context.Background(), s)
	if len(got.Records) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, record := range got.Records {
		if record.MachineID == cfg.MachineID && record.Name != cfg.MachineName {
			t.Fatalf("published name=%q", record.Name)
		}
	}
}

func TestMachinesTextShowsUnverifiedPairingAndSharedIdentity(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	s := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return s, nil }
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "source", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}}
	must(t, config.Save(home, cfg))
	source, err := machines.Build(cfg, "linux/amd64", "dev", "", time.Now())
	must(t, err)
	must(t, machines.Publish(context.Background(), s, source))
	paired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	shared := source
	shared.Name = "recipient"
	shared.MachineID = strings.Repeat("b", 32)
	shared.PairedAt = &paired
	shared.Credential = machines.CredentialBinding{Kind: config.MachineAssignmentR2Shared, AccessKeyID: "synthetic", SharedWith: source.MachineID}
	must(t, machines.Publish(context.Background(), s, shared))
	var out bytes.Buffer
	if code := Run([]string{"machines"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("exit=%d %s", code, &out)
	}
	for _, text := range []string{"Paired 2026-10-01", "Paired unknown", "shared R2 key with source (" + source.MachineID + ")", "cannot revoke independently", "untrusted claims", "Heartbeat"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing %q\n%s", text, &out)
		}
	}
	must(t, s.Delete(context.Background(), "machines/"+source.MachineID+".json"))
	out.Reset()
	if code := Run([]string{"machines"}, nil, &out, &out, env); code != 0 || !strings.Contains(out.String(), "shared R2 key with "+source.MachineID+" (claim") {
		t.Fatalf("missing source identity fallback: %d %s", code, &out)
	}
}

type unavailableMachineNames struct{ *storagetest.MemoryStore }

func (s unavailableMachineNames) ListPage(context.Context, string, string, int32) (storage.ObjectPage, error) {
	return storage.ObjectPage{}, errors.New("synthetic unavailable listing")
}

func TestFirstSetupNameKeepsDefaultWithoutIOAndRefusesPartialObservation(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	cfg := config.Config{MachineName: "earlier-choice"}
	var out bytes.Buffer
	must(t, chooseSetupMachineName(newPrompter(strings.NewReader("\n"), &out), &cfg, env))
	if cfg.MachineName != "" {
		t.Fatal("blank did not restore neutral default")
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return unavailableMachineNames{storagetest.NewMemoryStore()}, nil
	}
	cfg.MachineName = "earlier-choice"
	err := chooseSetupMachineName(newPrompter(strings.NewReader("new-choice\n"), &out), &cfg, env)
	if err == nil || cfg.MachineName != "earlier-choice" {
		t.Fatalf("partial observation changed label: %q %v", cfg.MachineName, err)
	}
}

// Naming is one grouped setup prompt. Syntax and duplicate retries use the same
// buffered input; EOF and incomplete observation must not change the draft.
// Regression: 2026-10 setup review P2-R1-06.
func TestSetupMachineNameGroupsValidationAndPreservesBufferedInput(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	store := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "taken", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic"}}
	record, err := machines.Build(cfg, "linux/amd64", "dev", "", time.Now())
	must(t, err)
	must(t, machines.Publish(t.Context(), store, record))
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("Bad Name\ntaken\nnew-name\nnext\n"), &out)
	defer p.close()
	must(t, chooseSetupMachineName(p, &cfg, env))
	next, err := p.in.ReadString('\n')
	must(t, err)
	if cfg.MachineName != "new-name" || next != "next\n" || strings.Count(out.String(), "? Name this machine") != 3 || !strings.Contains(out.String(), "name is already used") || setupReceiptIndex(out.String(), "Machine new-name") < 0 || setupReceiptIndex(out.String(), "Machine taken") >= 0 {
		t.Fatalf("name=%s next=%q\n%s", cfg.MachineName, next, &out)
	}
	pEOF := newPrompter(strings.NewReader(""), &out)
	defer pEOF.close()
	if err := chooseSetupMachineName(pEOF, &cfg, env); err == nil || cfg.MachineName != "new-name" {
		t.Fatalf("EOF changed name: %s %v", cfg.MachineName, err)
	}
}
