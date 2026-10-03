package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func TestSetupRollbackPreservesCodexPolicyAndSourceConsentFloors(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("original", "us-east-1", "profile", true, false, false, t.TempDir()))
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	env.Now = func() time.Time { return at }
	cfg := mustLoadConfig(t, home)
	cfg.Paused = true
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}
	cfg.CodexCapture = &config.CodexCaptureConfig{Scope: config.CodexAllProjects}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	cfg = mustLoadConfig(t, home)
	sched := recordLaunchd(t, &env, "loaded")
	sched.beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic restart failure after policy apply") }
	next := cfg
	policy := *cfg.CodexCapture
	next.CodexCapture = &policy
	next.CodexCapture.Scope = config.CodexIncludedProjects
	next.Storage.Bucket = "replacement"
	d := *cfg.Discovery
	next.Discovery = &d
	next.Discovery.CodexHomes = []string{"/synthetic/new-codex"}
	next.NoSkills = !cfg.NoSkills
	env.Now = func() time.Time { return at.Add(time.Hour) }
	if err := applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env); err == nil {
		t.Fatal("rollback injection did not run")
	}
	loaded := mustLoadConfig(t, home)
	if !reflect.DeepEqual(loaded.CodexCapture, cfg.CodexCapture) || !reflect.DeepEqual(loaded.Discovery, cfg.Discovery) || !loaded.Paused || loaded.PauseGeneration != cfg.PauseGeneration {
		t.Fatal("rollback lost policy/source/project consent")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	var document struct {
		SchemaVersion struct {
			Version int    `json:"version"`
			Writer  string `json:"writer"`
		} `json:"schema_version"`
	}
	must(t, json.Unmarshal(raw, &document))
	if document.SchemaVersion.Version != 3 || document.SchemaVersion.Writer != "codex-scope-floor-v3" {
		t.Fatal("rollback weakened policy floor fence")
	}
	if _, err := config.SetPaused(home, false, at.Add(-time.Nanosecond)); err == nil {
		t.Fatal("rollback forgot paused consent floor")
	}
	if _, err := config.SetPaused(home, false, at); err != nil {
		t.Fatal(err)
	}
}

func TestHookOnlyLegacyPolicyRollbackPreservesFloorWriter(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "edited-setup", true: "refresh"}[refresh], func(t *testing.T) {
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("original", "us-east-1", "profile", true, false, false, t.TempDir()))
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			env.Now = func() time.Time { return at }
			cfg := mustLoadConfig(t, home)
			cfg.Paused = true
			cfg.Discovery = nil
			cfg.CodexCapture = &config.CodexCaptureConfig{Scope: config.CodexAllProjects}
			must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
			must(t, config.Save(home, cfg))
			path := filepath.Join(home, "config.json")
			raw, err := os.ReadFile(path)
			must(t, err)
			var document map[string]json.RawMessage
			must(t, json.Unmarshal(raw, &document))
			document["schema_version"] = json.RawMessage(`{"version":3,"writer":"codex-scope-v3"}`)
			raw, err = json.Marshal(document)
			must(t, err)
			must(t, os.WriteFile(path, raw, 0600))
			cfg = mustLoadConfig(t, home)
			if cfg.Discovery != nil || !cfg.CodexCapture.Authorization.NativeStartFloor.Equal(at) {
				t.Fatal("legacy hook-only fixture lost its original policy floor")
			}
			attempted := false
			sched := recordLaunchd(t, &env, "loaded")
			sched.beforeLoad = func(scheduler.Ref) error {
				attempted = true
				return errors.New("synthetic restart failure after hook-only policy apply")
			}
			env.Now = func() time.Time { return at.Add(time.Hour) }
			if refresh {
				upgradedTo(t, &env)
				_, _, err = refreshSetup(env)
			} else {
				next := cfg
				policy := *cfg.CodexCapture
				next.CodexCapture = &policy
				next.CodexCapture.Scope = config.CodexIncludedProjects
				next.NoSkills = !cfg.NoSkills
				err = applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env)
			}
			if err == nil || !attempted {
				t.Fatalf("scheduler rollback injection was not reached: %v", err)
			}
			raw, err = os.ReadFile(path)
			must(t, err)
			must(t, json.Unmarshal(raw, &document))
			var writer struct {
				Version int    `json:"version"`
				Writer  string `json:"writer"`
			}
			must(t, json.Unmarshal(document["schema_version"], &writer))
			if writer.Version != 3 || writer.Writer != "codex-scope-floor-v3" {
				t.Fatalf("rollback restored a weaker hook-only writer: %+v", writer)
			}
			loaded := mustLoadConfig(t, home)
			if !reflect.DeepEqual(loaded.CodexCapture, cfg.CodexCapture) || !loaded.Paused || loaded.PauseGeneration != cfg.PauseGeneration || loaded.NoSkills != cfg.NoSkills || loaded.EffectiveSkillEvidence() != cfg.EffectiveSkillEvidence() || loaded.Discovery == nil || loaded.Discovery.Enabled || len(loaded.Discovery.Authorizations) != 0 {
				t.Fatal("rollback changed policy, consent, skill settings or discovery permission")
			}
			if _, err := config.SetPaused(home, false, at.Add(-time.Nanosecond)); err == nil {
				t.Fatal("rollback permitted resume before the original consent floor")
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(raw) != string(after) {
				t.Fatal("rejected resume changed the migrated rollback bytes")
			}
		})
	}
}
