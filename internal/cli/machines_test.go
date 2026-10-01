package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "laptop", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}}
	cfg.Archive.Enabled = true
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
	out := setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--project", project)
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
	cfg := config.Config{MachineID: strings.Repeat("a", 32), MachineName: "old", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}}
	cfg.Archive.Enabled = true
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
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--project", project)
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
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "codex", "--project", project)
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
