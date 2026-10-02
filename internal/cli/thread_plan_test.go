package cli

import (
	"bytes"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type planFault string

const (
	planRedirect planFault = "redirect"
	planPrior    planFault = "prior"
	planPresence planFault = "presence"
	planMutation planFault = "mutation"
	planDelete   planFault = "delete"
	planMode     planFault = "mode"
)

type faultyPlanHooks struct {
	syntheticHooks
	fault  planFault
	target string
}

func (h faultyPlanHooks) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	changes, err := h.syntheticHooks.Plan(r)
	if err != nil || len(changes) == 0 {
		return changes, err
	}
	c := &changes[0]
	switch h.fault {
	case planRedirect:
		c.Path = h.target
		c.Existed = false
		c.Before = nil
	case planPrior:
		c.Before = []byte("invented")
	case planPresence:
		c.Existed = !r.File.Present
	case planMutation:
		r.File.Bytes[0] = 'X'
		c.Before = append([]byte(nil), r.File.Bytes...)
	case planDelete:
		c.Delete = true
	case planMode:
		c.Mode = os.ModeDir | 0600
	}
	return changes, nil
}

func TestThreadTriageRejectsFaultyHookPlan(t *testing.T) {
	for _, action := range []agentapi.HookAction{agentapi.HookInstall, agentapi.HookRemove} {
		for _, fault := range []planFault{planRedirect, planPrior, planPresence, planMutation, planMode} {
			t.Run(string(rune('0'+action))+string(fault), func(t *testing.T) {
				home, userHome := t.TempDir(), t.TempDir()
				at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
				expected := filepath.Join(userHome, "native-weird", "signals.cfg")
				if err := os.MkdirAll(filepath.Dir(expected), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(expected, syntheticSettings, 0640); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(userHome, "unrelated")
				env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
				env.Agents = registryWithSyntheticHooks(t, faultyPlanHooks{syntheticHooks{}, fault, target})
				owner := env.installation(home, userHome).owner()
				files := hooks.Files{"synthetic": expected}
				var err error
				if action == agentapi.HookInstall {
					_, err = hooks.Plan(files, owner, []string{"synthetic"})
				} else {
					_, _, err = hooks.PlanRemovalOf(files, owner, "synthetic")
				}
				if err == nil {
					t.Fatal("host accepted substituted plan")
				}
				data, readErr := os.ReadFile(expected)
				if readErr != nil || !bytes.Equal(data, syntheticSettings) {
					t.Fatalf("settings changed %s %v", data, readErr)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("unrelated path changed: %v", err)
				}
			})
		}
	}
}

func TestFaultyHookPlanCannotEnterSetupJournal(t *testing.T) {
	for _, action := range []agentapi.HookAction{agentapi.HookInstall, agentapi.HookRemove} {
		t.Run(string(rune('0'+action)), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			expected := filepath.Join(userHome, "native-weird", "signals.cfg")
			if err := os.MkdirAll(filepath.Dir(expected), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(expected, syntheticSettings, 0640); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(userHome, "unrelated")
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			env.Agents = registryWithSyntheticHooks(t, faultyPlanHooks{syntheticHooks{}, planRedirect, target})
			old := config.Config{}
			next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine"}}
			if action == agentapi.HookRemove {
				old = next
				old.HookFiles = map[string]string{"synthetic": expected}
				next.Harnesses = nil
			}
			executable, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := applySetup(home, userHome, executable, old, &next, nil, env); err == nil {
				t.Fatal("shared setup accepted redirect")
			}
			if setupjournal.TransactionPending(home) {
				t.Fatal("invalid plan reached journal")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("redirect target %v", err)
			}
			data, err := os.ReadFile(expected)
			if err != nil || !bytes.Equal(data, syntheticSettings) {
				t.Fatalf("selected settings %s %v", data, err)
			}
		})
	}
}

func TestHookPlanReplacementModeAndDeletionFacts(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expected := filepath.Join(userHome, "native-weird", "signals.cfg")
	if err := os.MkdirAll(filepath.Dir(expected), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expected, syntheticSettings, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(expected, 0640); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registryWithSyntheticHooks(t, syntheticHooks{})
	files := hooks.Files{"synthetic": expected}
	plan, err := hooks.Plan(files, env.installation(home, userHome).owner(), []string{"synthetic"})
	if err != nil || len(plan) != 1 || plan[0].Mode != 0600 {
		t.Fatalf("allowed replacement mode %+v %v", plan, err)
	}
	if err := hooks.Apply(plan); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(expected)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement mode %v %v", info, err)
	}
	env.Agents = registryWithSyntheticHooks(t, faultyPlanHooks{syntheticHooks{}, planDelete, ""})
	if _, err := hooks.Plan(files, env.installation(home, userHome).owner(), []string{"synthetic"}); err == nil {
		t.Fatal("install deletion accepted")
	}
	target := filepath.Join(userHome, "target")
	if err := os.Rename(expected, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, expected); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hooks.PlanRemovalOf(files, env.installation(home, userHome).owner(), "synthetic"); err == nil {
		t.Fatal("symlink deletion accepted")
	}
}
