package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

type floorSetupChange string

const (
	floorSetupEnable      floorSetupChange = "enable"
	floorSetupDestination floorSetupChange = "destination"
	floorSetupHomes       floorSetupChange = "homes"
	floorSetupProject     floorSetupChange = "project"
)

func TestPausedSetupKeepsNativeConsentFloorThroughLockedResume(t *testing.T) {
	t.Parallel()
	for _, change := range []floorSetupChange{floorSetupEnable, floorSetupDestination, floorSetupHomes, floorSetupProject} {
		t.Run(string(change), func(t *testing.T) {
			t.Parallel()
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("original", "us-east-1", "profile", true, false, false, t.TempDir()))
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			env.Now = func() time.Time { return at }
			old := mustLoadConfig(t, home)
			if change != floorSetupEnable {
				old.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}
				must(t, config.ReconcileDiscovery(&old, config.Config{}, at.Add(-2*time.Hour)))
				must(t, config.Save(home, old))
			}
			var stdout, stderr bytes.Buffer
			if code := runPauseCommand(&stdout, &stderr, env, true); code != 0 {
				t.Fatal(stderr.String())
			}
			old = mustLoadConfig(t, home)
			next := old
			var changedRoot string
			switch change {
			case floorSetupEnable:
				next.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}
			case floorSetupDestination:
				next.Storage.Bucket = "replacement"
			case floorSetupHomes:
				d := *old.Discovery
				next.Discovery = &d
				next.Discovery.CodexHomes = []string{"/synthetic/new-codex"}
			case floorSetupProject:
				changedRoot = t.TempDir()
				next.Archive.Projects = append(append([]archive.ProjectActivation(nil), old.Archive.Projects...), archive.ProjectActivation{Root: changedRoot, ProjectID: archive.ProjectID(changedRoot), Included: true, ActivatedAt: at})
			}
			exe, err := env.executable()
			must(t, err)
			must(t, applySetup(home, userHome, exe, old, &next, nil, env))
			cfg := mustLoadConfig(t, home)
			if !cfg.Paused {
				t.Fatal("setup unpaused installation")
			}
			root := ""
			for _, a := range cfg.Discovery.Authorizations {
				// Query the committed canonical root so the assertion cannot pass
				// merely because a temporary path had a different spelling.
				if (change != floorSetupProject || filepath.Base(a.ProjectRoot) == filepath.Base(changedRoot)) && a.NativeStartFloor.Equal(at) {
					root = a.ProjectRoot
					if len(a.Intervals) != 0 {
						t.Fatalf("paused setup granted an interval: %+v", a)
					}
				}
			}
			if root == "" {
				t.Fatalf("new scope floor missing: %+v", cfg.Discovery)
			}
			path := filepath.Join(home, "config.json")
			before, err := os.ReadFile(path)
			must(t, err)
			env.Now = func() time.Time { return at.Add(-time.Hour) }
			stdout.Reset()
			stderr.Reset()
			if code := runPauseCommand(&stdout, &stderr, env, false); code == 0 {
				t.Fatal("locked CLI resumed behind committed setup consent")
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected CLI resume changed config, generation, or fence")
			}
			env.Now = func() time.Time { return at }
			stdout.Reset()
			stderr.Reset()
			if code := runPauseCommand(&stdout, &stderr, env, false); code != 0 {
				t.Fatal(stderr.String())
			}
			cfg = mustLoadConfig(t, home)
			if _, allowed := cfg.DiscoveryGeneration("codex", root, at.Add(-30*time.Minute), at.Add(time.Hour)); allowed {
				t.Fatal("setup widened native start permission")
			}
			if _, allowed := cfg.DiscoveryGeneration("codex", root, at, at.Add(time.Hour)); !allowed {
				t.Fatal("equality resume lost valid native start permission")
			}
		})
	}
}

func TestSetupRollbackPreservesPausedGenerationFloorAndWriterFence(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("original", "us-east-1", "profile", true, false, false, t.TempDir()))
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	env.Now = func() time.Time { return at }
	cfg := mustLoadConfig(t, home)
	cfg.Paused = true
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	cfg = mustLoadConfig(t, home)
	sched := recordLaunchd(t, &env, "loaded")
	sched.beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic restart failure after config apply") }
	next := cfg
	d := *cfg.Discovery
	next.Discovery = &d
	next.Discovery.CodexHomes = []string{"/synthetic/new-codex"}
	next.NoSkills = !cfg.NoSkills
	env.Now = func() time.Time { return at.Add(time.Hour) }
	if err := applySetup(home, userHome, cfg.InstalledExecutable, cfg, &next, nil, env); err == nil {
		t.Fatal("rollback injection did not run")
	}
	loaded := mustLoadConfig(t, home)
	if !reflect.DeepEqual(loaded.Discovery, cfg.Discovery) || !loaded.Paused || loaded.PauseGeneration != cfg.PauseGeneration {
		t.Fatal("rollback lost immutable paused consent")
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
	if document.SchemaVersion.Version != 2 || document.SchemaVersion.Writer != "discovery-floor-v2" {
		t.Fatal("rollback weakened floor writer fence")
	}
	if _, err := config.SetPaused(home, false, at.Add(-time.Nanosecond)); err == nil {
		t.Fatal("rollback forgot original native-start floor")
	}
}
