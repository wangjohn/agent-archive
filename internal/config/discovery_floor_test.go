package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPausedGenerationRetainsNativeStartFloorAcrossScopeChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"initial", "enable", "destination", "project", "homes"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			previous, at := discoveryConfig(t)
			previous.Paused = true
			previous.Discovery.Authorizations[0].Intervals[0].End = at.Add(time.Hour)
			next := previous
			d := *previous.Discovery
			next.Discovery = &d
			floor := at.Add(2 * time.Hour)
			root := "/included"
			switch change {
			case "initial":
				previous = Config{}
			case "enable":
				previous.Discovery = &DiscoveryConfig{Enabled: false}
				next.Discovery.Enabled = true
			case "destination":
				next.Storage.Bucket = "replacement"
				next.DestinationSince = floor
			case "project":
				root = "/new-project"
				next.Archive.Projects = append(append([]archive.ProjectActivation(nil), next.Archive.Projects...), archive.ProjectActivation{Root: root, Included: true, ActivatedAt: floor})
			case "homes":
				next.Discovery.CodexHomes = []string{"/new-codex-home"}
			}
			if err := ReconcileDiscovery(&next, previous, floor); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if err := Save(home, next); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := Load(home)
			if err != nil {
				t.Fatal(err)
			}
			var generation string
			for _, a := range loaded.Discovery.Authorizations {
				if a.ProjectRoot == root {
					generation = a.Generation
					if !a.NativeStartFloor.Equal(floor) || len(a.Intervals) != 0 {
						t.Fatalf("paused scope lost floor or gained permission: %+v", a)
					}
				}
			}
			if generation == "" {
				t.Fatal("scope missing")
			}
			path := filepath.Join(home, "config.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SetPaused(home, false, floor.Add(-time.Minute)); err == nil {
				t.Fatal("resume before floor succeeded")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("rejected multi-scope resume changed protected config: %v", err)
			}
			resumed, err := SetPaused(home, false, floor)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				start   time.Time
				allowed bool
			}{{floor.Add(-time.Nanosecond), false}, {floor, true}} {
				got, allowed := resumed.DiscoveryGeneration("codex", root, tc.start, floor.Add(time.Hour))
				if allowed != tc.allowed || (allowed && got != generation) {
					t.Fatalf("native start %v: %s %t", tc.start, got, allowed)
				}
			}
		})
	}
}

func legacyDiscoveryDocument(t *testing.T, cfg Config) []byte {
	t.Helper()
	raw, err := json.Marshal(configJSON(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["schema_version"] = json.RawMessage(`{"version":2,"writer":"discovery-v2"}`)
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLegacyEmptyDiscoveryHistoryRequiresRenewedConsent(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	cfg.Paused = true
	cfg.Discovery.Authorizations[0].Intervals = nil
	cfg.Discovery.Authorizations[0].NativeStartFloor = time.Time{}
	oldGeneration := cfg.Discovery.Authorizations[0].Generation
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	raw := legacyDiscoveryDocument(t, cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Even a forward clock cannot prove the vanished consent boundary.
	for _, resume := range []time.Time{at.Add(-time.Hour), at.Add(time.Hour)} {
		if _, err := SetPaused(home, false, resume); err == nil {
			t.Fatal("unknown old boundary resumed")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(raw) {
			t.Fatalf("refused resume rewrote old protected state: %v", err)
		}
	}
	if err := ProtectIdentityWriter(home); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Discovery.Authorizations[0].NativeStartFloor.IsZero() {
		t.Fatal("identity migration invented a boundary")
	}
	if _, err := SetPaused(home, false, at.Add(time.Hour)); err == nil {
		t.Fatal("writer migration granted unknown consent")
	}
	renewed := loaded
	floor := at.Add(2 * time.Hour)
	if err := ReconcileDiscovery(&renewed, loaded, floor); err != nil {
		t.Fatal(err)
	}
	a := renewed.Discovery.Authorizations[0]
	if a.Generation == oldGeneration || !a.NativeStartFloor.Equal(floor) || len(a.Intervals) != 0 {
		t.Fatalf("renewal did not commit fresh paused consent: %+v", a)
	}
	if err := Save(home, renewed); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, false, floor.Add(-time.Nanosecond)); err == nil {
		t.Fatal("renewal resumed below new floor")
	}
	if _, err := SetPaused(home, false, floor); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRetainedHistoryMigratesWithoutWideningConsent(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	cfg.Discovery.Authorizations[0].NativeStartFloor = time.Time{}
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, legacyDiscoveryDocument(t, cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectIdentityWriter(home); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Discovery.Authorizations[0].NativeStartFloor.Equal(at) {
		t.Fatal("retained history lost conservative floor")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prior struct {
		SchemaVersion writerVersion `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &prior); err != nil {
		t.Fatal(err)
	}
	// The exact prior protected decoder accepts only writer discovery-v2.
	if prior.SchemaVersion.Writer == legacyDiscoveryWriter || prior.SchemaVersion.Version != 2 {
		t.Fatalf("prior protected writer can flatten floor: %+v", prior.SchemaVersion)
	}
	var published struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(raw, &published) == nil {
		t.Fatal("published integer reader accepted floor-bearing state")
	}
	if _, allowed := loaded.DiscoveryGeneration("codex", "/included", at.Add(-time.Nanosecond), at); allowed {
		t.Fatal("migration widened native permission")
	}
}

func TestDiscoveryHistoryCompactionPreservesImmutableFloor(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	a := &cfg.Discovery.Authorizations[0]
	a.Intervals = nil
	for i := range 256 {
		start := at.Add(time.Duration(2*i) * time.Minute)
		a.Intervals = append(a.Intervals, DiscoveryInterval{Start: start, End: start.Add(time.Minute)})
	}
	cfg.Paused = true
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	resumeAt := at.Add(512 * time.Minute)
	resumed, err := SetPaused(home, false, resumeAt)
	if err != nil {
		t.Fatal(err)
	}
	a = &resumed.Discovery.Authorizations[0]
	if len(a.Intervals) != 256 || !a.NativeStartFloor.Equal(at) {
		t.Fatalf("compaction lost bound: %+v", a)
	}
	if _, allowed := resumed.DiscoveryGeneration("codex", "/included", at, resumeAt); allowed {
		t.Fatal("expired retained permission reopened")
	}
	if _, allowed := resumed.DiscoveryGeneration("codex", "/included", resumeAt, resumeAt); !allowed {
		t.Fatal("forward resumed permission missing")
	}
	if _, err := SetPaused(home, true, resumeAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, false, at.Add(time.Minute)); err == nil {
		t.Fatal("compacted history allowed backward resume")
	}
}

func TestPausedDiscoveryFloorUsesLatestPermissionBoundary(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"reconciliation", "activation", "destination"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			cfg, at := discoveryConfig(t)
			cfg.Paused = true
			cfg.Discovery.Authorizations = nil
			now, floor := at, at
			switch boundary {
			case "reconciliation":
				now = at.Add(time.Hour)
				floor = now
			case "activation":
				cfg.Archive.Projects[0].ActivatedAt = at.Add(time.Hour)
				floor = cfg.Archive.Projects[0].ActivatedAt
			case "destination":
				cfg.DestinationSince = at.Add(time.Hour)
				floor = cfg.DestinationSince
			}
			if err := ReconcileDiscovery(&cfg, Config{}, now); err != nil {
				t.Fatal(err)
			}
			if !cfg.Discovery.Authorizations[0].NativeStartFloor.Equal(floor) || len(cfg.Discovery.Authorizations[0].Intervals) != 0 {
				t.Fatalf("missing strongest permission boundary: %+v", cfg.Discovery)
			}
			home := t.TempDir()
			if err := Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := SetPaused(home, false, floor.Add(-time.Nanosecond)); err == nil {
				t.Fatal("resumed behind strongest permission boundary")
			}
			if _, err := SetPaused(home, false, floor); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFloorWriterRejectsMissingFloorInPermissionHistory(t *testing.T) {
	t.Parallel()
	cfg, _ := discoveryConfig(t)
	cfg.Discovery.Authorizations[0].NativeStartFloor = time.Time{}
	raw, err := json.Marshal(configJSON(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["schema_version"] = json.RawMessage(`{"version":2,"writer":"discovery-floor-v2"}`)
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err == nil {
		t.Fatal("current writer lost its promised durable floor")
	}
}
