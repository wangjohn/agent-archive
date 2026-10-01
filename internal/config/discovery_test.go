package config

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discoveryConfig(t *testing.T) (Config, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{Harnesses: []string{"codex"}, SkillEvidence: SkillEvidenceMetadata, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/included", Included: true, ActivatedAt: at}}}, Discovery: &DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}}
	if err := ReconcileDiscovery(&cfg, Config{}, at); err != nil {
		t.Fatal(err)
	}
	return cfg, at
}
func TestDiscoveryIntervalsExcludePauseAndKeepDelayedAuthorizedStarts(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	_, err := SetPausedAt(home, true, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = SetPausedAt(home, false, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		start   time.Time
		allowed bool
	}{{at.Add(-time.Nanosecond), false}, {at, true}, {at.Add(time.Hour - time.Nanosecond), true}, {at.Add(time.Hour), false}, {at.Add(90 * time.Minute), false}, {at.Add(2 * time.Hour), true}, {at.Add(4 * time.Hour), false}} {
		_, ok := cfg.DiscoveryGeneration("codex", "/included", tc.start, at.Add(3*time.Hour))
		if ok != tc.allowed {
			t.Errorf("start %v: %t", tc.start, ok)
		}
	}
}
func TestDiscoveryScopeChangesRotateForwardOnlyGenerations(t *testing.T) {
	t.Parallel()
	old, at := discoveryConfig(t)
	for _, mode := range []string{"disable", "reinclude", "destination", "exclusion", "home", "agent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			next := old
			d := *old.Discovery
			next.Discovery = &d
			next.Archive.Projects = append([]archive.ProjectActivation(nil), old.Archive.Projects...)
			switch mode {
			case "disable":
				next.Discovery.Enabled = false
			case "reinclude":
				next.Archive.Projects[0].Included = false
			case "destination":
				next.Storage.Bucket = "new"
			case "exclusion":
				next.Archive.Projects = append(next.Archive.Projects, archive.ProjectActivation{Root: "/included/private", Included: false})
			case "home":
				next.Discovery.CodexHomes = []string{"/other"}
			case "agent":
				next.Harnesses = []string{"claude"}
			}
			if err := ReconcileDiscovery(&next, old, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if mode == "disable" || mode == "reinclude" || mode == "agent" {
				prior := next
				next = old
				next.Discovery = &d
				next.Discovery.Enabled = true
				if err := ReconcileDiscovery(&next, prior, at.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if _, allowed := next.DiscoveryGeneration("codex", "/included", at.Add(time.Minute), at.Add(3*time.Hour)); allowed {
				t.Fatal("old start authorized after scope change")
			}
			if len(next.Discovery.Authorizations) > 0 && next.Discovery.Authorizations[0].Generation == old.Discovery.Authorizations[0].Generation {
				t.Fatal("generation reused")
			}
		})
	}
}
func TestDiscoveryWriterMarkerPreservesPolicyAndRejectsOldWriters(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	for _, policy := range []SkillEvidence{SkillEvidenceNone, SkillEvidenceMetadata, SkillEvidenceBody} {
		cfg.SkillEvidence = policy
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, _, err := Load(home)
		if err != nil || loaded.EffectiveSkillEvidence() != policy {
			t.Fatalf("policy %q: %v", policy, err)
		}
		// This is the base binary's exact supported-enum check, before any write.
		if ValidSkillEvidence(loaded.SkillEvidence) {
			t.Fatal("old writer would accept and erase authorization")
		}
		loaded.Discovery.Enabled = false
		if err := ReconcileDiscovery(&loaded, cfg, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := Save(home, loaded); err != nil {
			t.Fatal(err)
		}
		loaded, _, err = Load(home)
		if err != nil || !strings.HasSuffix(string(loaded.SkillEvidence), discoveryWriterMarker) {
			t.Fatal("disable erased rollback guard")
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, "config.json"))
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "discovery")
	raw, _ = json.Marshal(document)
	if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(home); err == nil {
		t.Fatal("marker without authorization accepted")
	}
}
func TestDiscoveryClockReversalCannotPartiallyResume(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPausedAt(home, true, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPausedAt(home, false, at.Add(time.Minute)); err == nil {
		t.Fatal("backwards clock resumed")
	}
	cfg, _, err := Load(home)
	if err != nil || !cfg.Paused {
		t.Fatal("failed resume changed committed permission")
	}
}
