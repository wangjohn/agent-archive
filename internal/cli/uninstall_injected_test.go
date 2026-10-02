package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/hooks"
)

func TestInjectedSetupRecordedHooksReachNormalUninstall(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registryWithSyntheticHooks(t, syntheticHooks{})
	executable, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine"}}
	if err := applySetup(home, userHome, executable, config.Config{}, &next, nil, env); err != nil {
		t.Fatal(err)
	}
	saved, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("saved config found=%v err=%v", found, err)
	}
	expected := filepath.Join(userHome, "native-weird", "signals.cfg")
	if saved.HookFiles["synthetic"] != expected {
		t.Fatalf("recorded paths=%v", saved.HookFiles)
	}
	before, err := os.ReadFile(expected)
	if err != nil || !bytes.Equal(before, syntheticSettings) {
		t.Fatalf("setup bytes=%q err=%v", before, err)
	}
	changes, skipped, err := planUninstallFiles(userHome, env.installedHookFiles(userHome, saved), env.installation(home, userHome), installedApps(saved, true))
	if err != nil {
		t.Fatal(err)
	}
	planned := false
	for _, change := range changes {
		if change.Path == expected {
			planned = true
		}
	}
	t.Logf("actual setup recorded synthetic settings; normal uninstall changes=%d planned_synthetic=%t skipped=%v", len(changes), planned, skipped)
	if err := hooks.Apply(changes); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(expected)
	if !planned || err == nil && bytes.Equal(after, syntheticSettings) {
		t.Fatalf("NMP3B-UNINSTALL-1: normal uninstall omitted installed injected settings; planned=%t native settings survive=%t", planned, err == nil && bytes.Equal(after, syntheticSettings))
	}
}

func TestInjectedUninstallRecordedAndLegacyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		malformed bool
		installed bool
		foreign   bool
		alias     bool
	}{
		{name: "owned-current-and-legacy", installed: true},
		{name: "foreign-current", installed: true, foreign: true},
		{name: "installed-unreadable", installed: true, malformed: true},
		{name: "optional-unreadable", malformed: true},
		{name: "same-file-alias", installed: true, alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			env.Agents = registryWithSyntheticHooks(t, syntheticHooks{})
			legacy := filepath.Join(userHome, "native-weird", "signals.cfg")
			current := filepath.Join(userHome, "recorded", "signals.cfg")
			for _, path := range []string{legacy, current} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(legacy, syntheticSettings, 0600); err != nil {
				t.Fatal(err)
			}
			data := syntheticSettings
			if tc.foreign {
				data = []byte(`{"peculiar":{"knock":"foreign"}}`)
			}
			switch {
			case tc.malformed:
				if err := os.Mkdir(current, 0700); err != nil {
					t.Fatal(err)
				}
			case tc.alias:
				if err := os.Symlink(legacy, current); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(current, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{HookFiles: map[string]string{"synthetic": current}}
			var installed []string
			if tc.installed {
				installed = []string{"synthetic"}
			}
			files := env.installedHookFiles(userHome, cfg)
			if files["synthetic"] != current {
				t.Fatal("recorded path lost precedence")
			}
			changes, skipped, err := planUninstallFiles(userHome, files, env.installation(home, userHome), installed)
			if tc.malformed && tc.installed {
				if err == nil {
					t.Fatal("unreadable installed settings silently skipped")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.malformed && len(skipped) != 1 {
				t.Fatalf("optional unreadable skip=%v", skipped)
			}
			want := 2
			if tc.foreign || tc.malformed || tc.alias {
				want = 1
			}
			if len(changes) != want {
				t.Fatalf("changes=%+v want=%d", changes, want)
			}
			if tc.alias && changes[0].Path != current {
				t.Fatal("recorded alias lost precedence")
			}
			if err := hooks.Apply(changes); err != nil {
				t.Fatal(err)
			}
			if tc.foreign {
				after, err := os.ReadFile(current)
				if err != nil || !bytes.Equal(after, data) {
					t.Fatalf("foreign settings changed %q %v", after, err)
				}
			}
			apps := installedApps(config.Config{}, true, env.agentRegistry())
			if !containsString(apps, "synthetic") || apps[0] != "codex" {
				t.Fatalf("empty config compatibility/order=%v", apps)
			}
		})
	}
}

type sharedRemovalHooks struct {
	syntheticHooks
	after []byte
}

func (h sharedRemovalHooks) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	return []filechange.Change{{Path: r.File.Path, Before: r.File.Bytes, After: h.after, Existed: r.File.Present, Mode: r.File.Mode}}, nil
}

type sharedRemovalLookup struct {
	first  agentapi.HookConfigurator
	second agentapi.HookConfigurator
}

func (p sharedRemovalLookup) HookAgents() []string { return []string{"first", "second"} }

func (p sharedRemovalLookup) LookupHooks(name string) (agentapi.HookConfigurator, bool) {
	if name == "first" {
		return p.first, true
	}
	if name == "second" {
		return p.second, true
	}
	return nil, false
}

func TestUninstallSharedFileOwnersRequireAgreeingPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(path, []byte("both owners"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, agree := range []bool{true, false} {
		second := []byte("removed")
		if !agree {
			second = []byte("other owner retained")
		}
		ports := sharedRemovalLookup{first: sharedRemovalHooks{after: []byte("removed")}, second: sharedRemovalHooks{after: second}}
		changes, _, err := planUninstallHooks(hooks.Files{"first": path, "second": path}, nil, hooks.Hook{Ports: ports}, []string{"first", "second"})
		if agree {
			if err != nil || len(changes) != 1 {
				t.Fatalf("agreeing plans=%v err=%v", changes, err)
			}
		} else if err == nil {
			t.Fatal("conflicting owner plan silently dropped")
		}
	}
}

func TestUninstallRecordedInstalledOwnerWithoutFormatterFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings")
	if err := os.WriteFile(path, []byte("unrecognized owned settings"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{HookFiles: map[string]string{"retired-owner": path}}
	installed := installedApps(cfg, true, productionAgents)
	if !containsString(installed, "retired-owner") {
		t.Fatal("empty configured list omitted recorded installed owner")
	}
	if _, _, err := planUninstallHooks(hooks.Files(cfg.HookFiles), nil, hooks.Hook{Ports: productionAgents}, []string{"retired-owner"}); err == nil {
		t.Fatal("installed recorded owner without formatter silently skipped")
	}
	if _, skipped, err := planUninstallHooks(hooks.Files(cfg.HookFiles), nil, hooks.Hook{Ports: productionAgents}, nil); err != nil || len(skipped) != 1 {
		t.Fatalf("optional leftover policy skipped=%v err=%v", skipped, err)
	}
}
