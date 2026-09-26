package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// relativeKeyStore rejects absolute keys on every operation, mirroring the
// S3 store's prefix composition, so a probe that passes a pre-prefixed key
// fails the same way it would against a configured bucket.
type relativeKeyStore struct{ *storagetest.MemoryStore }

func (s relativeKeyStore) Put(ctx context.Context, key string, data []byte) error {
	if err := requireRelativeKey(key); err != nil {
		return err
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func (s relativeKeyStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := requireRelativeKey(key); err != nil {
		return nil, err
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s relativeKeyStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if prefix != "" {
		if err := requireRelativeKey(prefix); err != nil {
			return nil, err
		}
	}
	return s.MemoryStore.List(ctx, prefix)
}

func (s relativeKeyStore) Delete(ctx context.Context, key string) error {
	if err := requireRelativeKey(key); err != nil {
		return err
	}
	return s.MemoryStore.Delete(ctx, key)
}

func requireRelativeKey(key string) error {
	_, err := storage.Prefix("configured-prefix", key)
	return err
}

func TestScheduledProbeContinuesToPublication(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s1", NativeSessionID: "n1", ProjectID: "p1", ProjectRoot: project, Harness: archive.Harness{Name: "codex"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := relativeKeyStore{storagetest.NewMemoryStore()}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	result, err := runOnePass(env, true)
	if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
		t.Fatalf("scheduled publication: %+v, %v", result, err)
	}
	evidence, err := readVerification(home, "s1")
	if err != nil || evidence.VerifiedAt.IsZero() {
		t.Fatalf("readback: %+v %v", evidence, err)
	}
	objects, err := remote.List(context.Background(), ".setup-test/")
	if err != nil || len(objects) != 0 {
		t.Fatalf("probe cleanup: %v %v", objects, err)
	}
}

// processFailingStore fails every request the way the S3 client does when
// the profile's credential_process fails, with what the process printed in
// the SDK's error.
type processFailingStore struct{ *storagetest.MemoryStore }

func (processFailingStore) Put(context.Context, string, []byte) error {
	return fmt.Errorf("operation error S3: PutObject, failed to retrieve credentials: %w",
		&processcreds.ProviderError{Err: errors.New("parse failed of process output: {\"SecretAccessKey\":\"printed-secret\"}")})
}

// A background pass whose credential_process fails records that, in words
// status recognizes, and never what the process printed.
func TestBackgroundPassRecordsACredentialProcessFailure(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return processFailingStore{storagetest.NewMemoryStore()}, nil
	}
	if _, err := runOnePass(env, true); err == nil {
		t.Fatal("the pass succeeded without credentials")
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := localStore.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.LastError != backgroundCredentialProcessFailure {
		t.Fatalf("last error %q", status.LastError)
	}
	var health storageHealth
	if err := local.Read(filepath.Join(home, "storage-health.json"), &health); err != nil || health.State != "credentials_unavailable" {
		t.Fatalf("storage health %+v %v", health, err)
	}
	for _, name := range []string{"status.json", "storage-health.json"} {
		if data, _ := os.ReadFile(filepath.Join(home, name)); strings.Contains(string(data), "printed-secret") {
			t.Fatalf("%s holds what the credential_process printed", name)
		}
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Needs attention" || !strings.Contains(view.Next, "couldn't get credentials from your AWS profile's credential_process") {
		t.Fatalf("state %q, next %q", view.State, view.Next)
	}
}
