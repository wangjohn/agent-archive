package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestCodexConsentFloorAndAtomicPause(t *testing.T) {
	c, at := blanketConfig(t)
	// A source consent boundary later than the blanket boundary must prevent
	// every scope from changing when pause is rejected.
	c.CodexCapture.SourceAuthorization.Intervals[0].Start = at.Add(time.Minute)
	home := t.TempDir()
	if err := Save(home, c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if _, err := SetPaused(home, true, at.Add(time.Minute)); err == nil {
		t.Fatal("equal source boundary accepted")
	}
	after, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if string(before) != string(after) {
		t.Fatal("rejected transition changed bytes")
	}
	if err := transitionDiscoveryPause(&c, true, at.Add(time.Minute)); err == nil {
		t.Fatal("in-memory invalid transition accepted")
	}
	if !c.CodexCapture.Authorization.Intervals[0].End.IsZero() {
		t.Fatal("earlier scope mutated before source refusal")
	}
}

func TestCodexPausedConsentFloorAndUnknownRenewal(t *testing.T) {
	c, at := blanketConfig(t)
	c.Paused = true
	c.CodexCapture.Authorization = nil
	c.CodexCapture.SourceAuthorization = nil
	c.Discovery.Authorizations = nil
	c.DestinationSince = at.Add(time.Hour)
	if err := ReconcileDiscovery(&c, Config{}, at); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		if !a.NativeStartFloor.Equal(c.DestinationSince) || len(a.Intervals) != 0 {
			t.Fatal("paused policy/source lost latest boundary")
		}
	}
	home := t.TempDir()
	if err := Save(home, c); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, false, at.Add(time.Minute)); err == nil {
		t.Fatal("resumed below consent floor")
	}
	if _, err := SetPaused(home, false, c.DestinationSince); err != nil {
		t.Fatal(err)
	}
	c.CodexCapture.Authorization.NativeStartFloor = time.Time{}
	c.CodexCapture.SourceAuthorization.NativeStartFloor = time.Time{}
	if err := transitionDiscoveryPause(&c, false, at.Add(2*time.Hour)); err == nil {
		t.Fatal("unknown history resumed")
	}
	old := c
	if err := ReconcileDiscovery(&c, old, at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		if !a.NativeStartFloor.Equal(at.Add(3 * time.Hour)) {
			t.Fatal("explicit renewal did not establish floor")
		}
	}
	if c.CodexCapture.Authorization.Generation == old.CodexCapture.Authorization.Generation {
		t.Fatal("unknown generation reused")
	}
}

func TestCodexFloorWriterMigrationIsPrivateAndProtected(t *testing.T) {
	c, at := blanketConfig(t)
	c.CodexCapture.Authorization.NativeStartFloor = time.Time{}
	c.CodexCapture.SourceAuthorization.NativeStartFloor = time.Time{}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !c.CodexCapture.Authorization.NativeStartFloor.IsZero() {
		t.Fatal("marshal mutated caller")
	}
	var wire struct {
		SchemaVersion writerVersion `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.SchemaVersion.Writer == legacyCodexWriter {
		t.Fatal("prior policy writer could discard floor")
	}
	var loaded Config
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if !loaded.CodexCapture.Authorization.NativeStartFloor.Equal(at) {
		t.Fatal("missing migrated floor")
	}
	const nestedMarkers SkillEvidence = "none+discovery-v2+codex-scope-v3"
	loaded.SkillEvidence = nestedMarkers
	raw, err = json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.EffectiveSkillEvidence() != SkillEvidenceNone {
		t.Fatal("nested marker normalization lost evidence mode")
	}
}

func TestCodexLegacyPolicyMigrationAndMissingFloorRefusal(t *testing.T) {
	c, at := blanketConfig(t)
	c.CodexCapture.Authorization.NativeStartFloor = time.Time{}
	c.CodexCapture.SourceAuthorization.NativeStartFloor = time.Time{}
	raw, err := json.Marshal(configJSON(c))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema_version"] = json.RawMessage(`{"version":3,"writer":"codex-scope-v3"}`)
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectIdentityWriter(home); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CodexCapture.Authorization.NativeStartFloor.Equal(at) || !got.CodexCapture.SourceAuthorization.NativeStartFloor.Equal(at) {
		t.Fatal("old policy retained histories did not migrate")
	}
	doc["schema_version"] = json.RawMessage(`{"version":3,"writer":"codex-scope-floor-v3"}`)
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err == nil {
		t.Fatal("floor writer accepted missing promised floors")
	}
}

func TestCodexHistoryCompactionAndRejectedReconciliationPreserveSnapshots(t *testing.T) {
	c, at := blanketConfig(t)
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		a.Intervals = nil
		for i := range 256 {
			start := at.Add(time.Duration(i*2) * time.Minute)
			a.Intervals = append(a.Intervals, DiscoveryInterval{Start: start, End: start.Add(time.Minute)})
		}
	}
	c.Paused = true
	old := c
	before, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := transitionDiscoveryPause(&c, false, at.Add(512*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(old)
	if err != nil || string(before) != string(after) {
		t.Fatal("transition mutated old snapshot")
	}
	c.Paused = false
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		if len(a.Intervals) != 256 || !a.NativeStartFloor.Equal(at) {
			t.Fatal("compaction changed floor")
		}
	}
	next := c
	next.Paused = true
	nextBefore, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDiscovery(&next, c, at.Add(512*time.Minute)); err == nil {
		t.Fatal("equal reconciliation boundary accepted")
	}
	nextAfter, err := json.Marshal(next)
	if err != nil || string(nextBefore) != string(nextAfter) {
		t.Fatal("rejected reconciliation mutated caller")
	}
}

func TestCodexSubtreeBarrierNeverMovesBackward(t *testing.T) {
	c, at := blanketConfig(t)
	c.Archive.Projects = []archive.ProjectActivation{{Root: "/excluded", Included: false}}
	c.CodexCapture.Barriers = []CodexRuleBarrier{{Root: "/excluded", Since: at.Add(2 * time.Hour)}}
	old := c
	c.Archive.Projects = nil
	if err := ReconcileDiscovery(&c, old, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !c.CodexCapture.Barriers[0].Since.Equal(at.Add(2 * time.Hour)) {
		t.Fatal("backward exception edit lowered durable subtree barrier")
	}
	if _, ok := c.CodexGeneration("/excluded", "/excluded", at.Add(90*time.Minute), at.Add(3*time.Hour)); ok {
		t.Fatal("backward lift admitted excluded-period history")
	}
}

func TestNativeConsentStartKeepsDestinationAndProjectMaximum(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, scope := range []CodexCaptureScope{CodexIncludedProjects, CodexAllProjects} {
			t.Run(string(scope)+map[bool]string{false: "/active", true: "/paused"}[paused], func(t *testing.T) {
				c, at := blanketConfig(t)
				setTestCodexScope(&c, scope)
				c.Paused = paused
				c.DestinationSince = at.Add(time.Hour)
				c.Archive.Projects = []archive.ProjectActivation{{Root: "/later", Included: true, ActivatedAt: at.Add(2 * time.Hour)}, {Root: "/earlier", Included: true, ActivatedAt: at.Add(-time.Hour)}}
				if err := ReconcileDiscovery(&c, Config{}, at); err != nil {
					t.Fatal(err)
				}
				for _, a := range c.Discovery.Authorizations {
					want := c.DestinationSince
					if a.ProjectRoot == "/later" {
						want = at.Add(2 * time.Hour)
					}
					if !a.NativeStartFloor.Equal(want) {
						t.Fatalf("project floor %s != %s", a.NativeStartFloor, want)
					}
					if paused && len(a.Intervals) != 0 {
						t.Fatal("paused project opened interval")
					}
				}
				if scope == CodexAllProjects {
					for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
						if !a.NativeStartFloor.Equal(c.DestinationSince) {
							t.Fatal("blanket/source floor lost future destination")
						}
						if paused && len(a.Intervals) != 0 {
							t.Fatal("paused blanket/source opened interval")
						}
					}
				}
			})
		}
	}
}

func TestNativeConsentMaximumDoesNotReplacePauseClock(t *testing.T) {
	c, at := blanketConfig(t)
	previous := c
	c.Paused = true
	c.DestinationSince = at.Add(time.Hour)
	before, _ := json.Marshal(c)
	if err := ReconcileDiscovery(&c, previous, at); err == nil {
		t.Fatal("future destination masked invalid pause equality")
	}
	after, _ := json.Marshal(c)
	if !bytes.Equal(before, after) {
		t.Fatal("failed pause changed caller")
	}
}
