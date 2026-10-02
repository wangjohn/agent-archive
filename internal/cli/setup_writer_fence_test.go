package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestSetupRollbackCannotRestoreNumericProtectedWriter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refresh bool
		enabled bool
	}{{"setup-disabled", false, false}, {"refresh-disabled", true, false}, {"setup-enabled", false, true}, {"refresh-enabled", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", true, true, false, t.TempDir()))
			// An earlier protected writer has already committed authoritative
			// identities, while its schema2 wire format was still numeric.
			store, err := state.Open(home)
			must(t, err)
			unlock, err := local.NamedLock(home, "hooks.lock")
			must(t, err)
			_, _, err = store.EnsureAgentSessionID("claude", "already-namespaced")
			unlock()
			must(t, err)
			cfg := mustLoadConfig(t, home)
			if cfg.Discovery == nil || cfg.Discovery.Enabled {
				t.Fatal("identity guard did not preserve disabled consent")
			}
			cfg.Discovery.Enabled = tc.enabled
			must(t, config.Save(home, cfg))
			raw, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			var document map[string]json.RawMessage
			must(t, json.Unmarshal(raw, &document))
			document["schema_version"] = json.RawMessage("2")
			raw, err = json.Marshal(document)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(home, "config.json"), raw, 0600))
			sched := recordLaunchd(t, &env, "loaded")
			sched.beforeLoad = func(scheduler.Ref) error { return errors.New("injected restart failure after config apply") }
			if tc.refresh {
				upgradedTo(t, &env)
				_, _, err = refreshSetup(env)
			} else {
				next := cfg
				next.NoSkills = !cfg.NoSkills
				err = applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env)
			}
			if err == nil {
				t.Fatal("transaction did not encounter injected rollback")
			}
			loaded := mustLoadConfig(t, home)
			if !reflect.DeepEqual(loaded, cfg) {
				t.Fatal("rollback changed protected consent or settings")
			}
			raw, err = os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			var published struct {
				SchemaVersion int `json:"schema_version"`
			}
			if json.Unmarshal(raw, &published) == nil {
				t.Fatal("rollback reopened the published numeric writer")
			}
		})
	}
}
