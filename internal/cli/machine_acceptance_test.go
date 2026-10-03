package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This exercises reachable commands across two entirely synthetic homes. It
// establishes local contracts; it says nothing about live provider propagation.
func TestMachineTwoHomeFakeAcceptance(t *testing.T) {
	source, _, _, _, _ := ownKeyFixture(t)
	store := storagetest.NewMemoryStore()
	source.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	lookup := source.LookupEnv
	source.LookupEnv = func(k string) (string, bool) {
		enabled := map[string]bool{"AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_REVOKE": true, "AGENT_ARCHIVE_EXPERIMENTAL_MACHINE_VERIFY": true}
		if enabled[k] {
			return "1", true
		}
		return lookup(k)
	}
	var output, diagnostics bytes.Buffer
	if code := Run([]string{"machines", "own-key", "--yes"}, nil, &output, &diagnostics, source); code != 0 {
		t.Fatalf("own-key %d %s", code, diagnostics.String())
	}
	output.Reset()
	if code := Run([]string{"machines", "add", "--name", "laptop", "--spares=1", "--yes"}, nil, &output, &diagnostics, source); code != 0 {
		t.Fatalf("add %d %s", code, diagnostics.String())
	}
	var bundle, secretCode string
	for line := range strings.SplitSeq(output.String(), "\n") {
		if strings.HasPrefix(line, "aa-pair1:") {
			bundle = line
		}
		if value, ok := strings.CutPrefix(line, "Pairing code (deliver separately): "); ok {
			secretCode = value
		}
	}
	if bundle == "" || secretCode == "" {
		t.Fatal("missing fake delivery")
	}
	home, userHome := t.TempDir(), t.TempDir()
	receiver := setupTestEnv(t, home, userHome, newFakeKeychain(), source.now())
	receiver.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	receiver.Cloudflare = source.Cloudflare
	receiver.DetectHarnesses = func(string) []string { return []string{"codex"} }
	receiver.LookupEnv = func(k string) (string, bool) {
		if k == "AGENT_ARCHIVE_PAIRING_CODE" {
			return secretCode, true
		}
		return source.LookupEnv(k)
	}
	receiver.UnsetEnv = func(string) error { return nil }
	project := filepath.Join(userHome, "project")
	must(t, os.MkdirAll(project, 0700))
	output.Reset()
	if code := Run([]string{"setup", "--pair-file", "-", "--yes", "--project", project}, strings.NewReader(bundle), &output, &diagnostics, receiver); code != 0 {
		t.Fatalf("pair %d %s", code, diagnostics.String())
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	originalID := cfg.MachineID
	output.Reset()
	if code := Run([]string{"machines", "rename", "portable"}, nil, &output, &diagnostics, receiver); code != 0 {
		t.Fatalf("rename %d %s", code, diagnostics.String())
	}
	cfg, _, err = config.Load(home)
	must(t, err)
	if cfg.MachineID != originalID {
		t.Fatal("rename altered authority")
	}
	output.Reset()
	if code := Run([]string{"machines", "--verify", "--json"}, nil, &output, &diagnostics, source); code != 0 {
		t.Fatalf("verify %d %s", code, diagnostics.String())
	}
	if !strings.Contains(output.String(), cfg.MachineID) {
		t.Fatal("paired machine missing")
	}
	output.Reset()
	if code := Run([]string{"machines", "revoke", "portable", "--yes"}, nil, &output, &diagnostics, receiver); code != 0 {
		t.Fatalf("revoke %d %s", code, diagnostics.String())
	}
	if !strings.Contains(output.String(), "Access removed for the verified key set") {
		t.Fatal("missing verified outcome")
	}
}
