package cli

import (
	"bytes"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	sharedInstallFirstID  agentmeta.ID = "first"
	sharedInstallSecondID agentmeta.ID = "second"
)

type sharedInstallCase string

const (
	sharedInstallIdentical    sharedInstallCase = "identical"
	sharedInstallAbsent       sharedInstallCase = "absent"
	sharedInstallByteConflict sharedInstallCase = "conflicting-bytes"
	sharedInstallModeConflict sharedInstallCase = "conflicting-mode"
	sharedInstallAlias        sharedInstallCase = "physical-alias"
	sharedInstallHardlink     sharedInstallCase = "hardlink-destinations"
	sharedInstallDifferent    sharedInstallCase = "different-locations"
)

// Regression SHARED-INSTALL-01: selected managed owners may share one native file.
type sharedInstallHooks struct {
	sharedRemovalHooks
	path string
}

func (h sharedInstallHooks) Location(agentapi.HookLocations) string { return h.path }

func TestSharedInstallPlansReachJournal(t *testing.T) {
	for _, kind := range []sharedInstallCase{sharedInstallIdentical, sharedInstallAbsent, sharedInstallByteConflict, sharedInstallModeConflict, sharedInstallAlias, sharedInstallHardlink, sharedInstallDifferent} {
		t.Run(string(kind), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(t.TempDir(), "settings")
			before := []byte("original shared settings")
			after := []byte("identical installed settings")
			if kind != sharedInstallAbsent {
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			secondPath := path
			secondAfter := after
			secondMode := os.FileMode(0600)
			if kind == sharedInstallByteConflict {
				secondAfter = []byte("conflicting installed settings")
			}
			if kind == sharedInstallModeConflict {
				secondMode = 0644
			}
			if kind == sharedInstallAlias {
				secondPath = filepath.Join(filepath.Dir(path), "alias")
				if err := os.Symlink(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			if kind == sharedInstallHardlink {
				secondPath = filepath.Join(filepath.Dir(path), "hardlink")
				if err := os.Link(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			if kind == sharedInstallDifferent {
				secondPath = filepath.Join(filepath.Dir(path), "other")
				if err := os.WriteFile(secondPath, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ports := sharedRemovalLookup{first: sharedRemovalHooks{after: after, mode: 0600}, second: sharedRemovalHooks{after: secondAfter, mode: secondMode}}
			plan, err := hooks.Plan(hooks.Files{"first": path, "second": secondPath}, hooks.Hook{Ports: ports}, []string{"first", "second"})
			if kind == sharedInstallByteConflict || kind == sharedInstallModeConflict {
				if err == nil || len(plan) != 0 {
					t.Errorf("conflicting plans accepted: plans=%d err=%v", len(plan), err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, before) || setupjournal.TransactionPending(home) {
					t.Fatal("conflict changed settings or journal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if kind == sharedInstallDifferent || kind == sharedInstallHardlink {
				want = 2
			}
			if len(plan) != want || plan[0].Path != path {
				t.Fatalf("plans=%+v want count=%d first path=%s", plan, want, path)
			}
			err = setupjournal.Commit(home, setupjournal.Journal{FilesOnly: true, Changes: plan}, nil)
			got, readErr := os.ReadFile(path)
			t.Logf("actual Plan/Commit: kind=%s plans=%d commitErr=%v beforeRetained=%t afterRetained=%t journalPending=%t", kind, len(plan), err, bytes.Equal(got, before), bytes.Equal(got, after), setupjournal.TransactionPending(home))
			if err != nil || readErr != nil || !bytes.Equal(got, after) || setupjournal.TransactionPending(home) {
				t.Errorf("agreeing selected owners cannot install: err=%v data=%q", err, got)
			}
			second, readErr := os.ReadFile(secondPath)
			if readErr != nil || !bytes.Equal(second, after) {
				t.Fatalf("second location bytes=%q err=%v", second, readErr)
			}
		})
	}
}

func TestSharedInstallOwnersReachActualSetup(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			path := filepath.Join(userHome, "shared-native", "settings")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("original shared settings")
			after := []byte("identical installed settings")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"})
			catalog, err := agentmeta.New(descriptors)
			if err != nil {
				t.Fatal(err)
			}
			var bindings []builtin.Integration
			for _, d := range agentmeta.Builtins().All() {
				b, _ := productionAgents.Lookup(string(d.ID))
				b.Descriptor = agentmeta.Descriptor{ID: d.ID}
				bindings = append(bindings, b)
			}
			for _, id := range []agentmeta.ID{sharedInstallFirstID, sharedInstallSecondID} {
				installed := after
				if conflict && id == sharedInstallSecondID {
					installed = []byte("conflicting installed settings")
				}
				bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: id}, Hooks: sharedInstallHooks{sharedRemovalHooks: sharedRemovalHooks{after: installed, mode: 0600}, path: path}})
			}
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			next := config.Config{MachineID: "machine", Harnesses: []string{"first", "second"}, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine"}}
			err = applySetup(home, userHome, exe, config.Config{}, &next, nil, env)
			got, readErr := os.ReadFile(path)
			_, found, cfgErr := config.Load(home)
			t.Logf("actual applySetup: err=%v beforeRetained=%t configCommitted=%t cfgErr=%v journalPending=%t", err, bytes.Equal(got, before), found, cfgErr, setupjournal.TransactionPending(home))
			if conflict {
				if err == nil || readErr != nil || !bytes.Equal(got, before) || found || cfgErr != nil || setupjournal.TransactionPending(home) {
					t.Fatalf("conflicting setup reached effects: err=%v bytes=%q committed=%t", err, got, found)
				}
				return
			}
			if err != nil || readErr != nil || !bytes.Equal(got, after) || !found || cfgErr != nil {
				t.Errorf("actual setup cannot install identical shared plans: %v", err)
			}
			saved, _, err := config.Load(home)
			if err != nil || saved.HookFiles["first"] != path || saved.HookFiles["second"] != path {
				t.Fatalf("selected ownership paths lost: %+v %v", saved.HookFiles, err)
			}

		})
	}
}

// Regression SHARED-UNINSTALL-HARDLINK-01: both rename destinations need removal.
func TestSharedUninstallHardlinkWriteDestinations(t *testing.T) {
	for _, sameOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameOwner=%t", sameOwner), func(t *testing.T) {
			dir := t.TempDir()
			first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
			before, after := []byte("before"), []byte("removed")
			if err := os.WriteFile(first, before, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(first, second); err != nil {
				t.Fatal(err)
			}
			ports := sharedRemovalLookup{first: sharedRemovalHooks{after: after, mode: 0600}, second: sharedRemovalHooks{after: after, mode: 0600}}
			files := hooks.Files{"first": first, "second": second}
			var legacy hooks.Files
			if sameOwner {
				files = hooks.Files{"first": first}
				legacy = hooks.Files{"first": second}
			}
			installed := []string{"first", "second"}
			if sameOwner {
				installed = []string{"first"}
			}
			changes, skipped, err := planUninstallHooks(files, legacy, hooks.Hook{Ports: ports}, installed)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 2 || len(skipped) != 0 {
				t.Fatalf("plans=%d skipped=%v", len(changes), skipped)
			}
			if err := hooks.Apply(changes); err != nil {
				t.Fatal(err)
			}
			left, _ := os.ReadFile(first)
			right, _ := os.ReadFile(second)
			t.Logf("sameOwner=%t plans=%d skipped=%v first=%q second=%q", sameOwner, len(changes), skipped, left, right)
			if !bytes.Equal(left, after) || !bytes.Equal(right, after) {
				t.Errorf("SHARED-UNINSTALL-HARDLINK-01: normal removal skipped distinct atomic-rename destination")
			}
		})
	}
}
