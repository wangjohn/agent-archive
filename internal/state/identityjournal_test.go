package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestIdentityJournalRepairsBothDeletedDerivedIndexes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	r, err := s.RegisterOrMerge("native-1", registrationFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"native-1", "claude\x00native-1"} {
		if err := os.Remove(nativeSessionIndexPath(s.home, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	again, err := s.RegisterOrMerge("native-1", registrationFor)
	if err != nil || again.ArchiveSessionID != r.ArchiveSessionID {
		t.Fatal("derived index loss duplicated registration", err)
	}
	regs, _ := s.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatal("duplicate registration")
	}
}

func TestLegacyMigrationIsBoundedAndGuardsRollbackBeforeIndexes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	c := config.Config{SkillEvidence: config.SkillEvidenceBody}
	if err := config.Save(s.home, c); err != nil {
		t.Fatal(err)
	}
	for i := range 90 {
		r := registrationFor(fmt.Sprintf("legacy-%03d", i))
		r.NativeSessionID = fmt.Sprintf("native-%03d", i)
		// Synthetic pre-upgrade state has no index, including the damaged-index case.
		if err := s.SaveRegistration(r); err != nil {
			t.Fatal(err)
		}
	}
	complete, err := s.ReconcileIdentityIndexes(1, nil)
	if err != nil || complete {
		t.Fatal("migration unexpectedly completed whole catalog", err)
	}
	loaded, _, err := config.Load(s.home)
	if err != nil || loaded.Discovery == nil || loaded.Discovery.Enabled || loaded.EffectiveSkillEvidence() != config.SkillEvidenceBody {
		t.Fatal("guard changed policy or consent", err)
	}
	if loaded.SkillEvidence == config.SkillEvidenceBody {
		t.Fatal("old validated-enum writer would accept new state")
	}
	if _, _, err := s.EnsureAgentSessionID("claude", "fresh"); !errors.Is(err, ErrIdentityMigrationPending) {
		t.Fatal("allocation widened incomplete migration", err)
	}
	for n := 0; n < 200 && !complete; n++ {
		complete, err = s.ReconcileIdentityIndexes(1, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !complete {
		t.Fatal("migration starved")
	}
	got, found, err := s.AgentSessionID("claude", "native-089")
	if err != nil || !found || got != "legacy-089" {
		t.Fatal("legacy registration identity lost", err)
	}
	if _, _, err := s.EnsureAgentSessionID("claude", "fresh"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.home, "config.json"))
	if len(raw) == 0 {
		t.Fatal("guard not durable")
	}
}

func TestCorruptJournalNeverAllocatesAReplacementIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	id, _, err := s.EnsureAgentSessionID("codex", "native")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(nativeSessionIndexPath(s.home, "codex\x00native")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.identityRecordPath("codex", "native"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if next, _, err := s.EnsureAgentSessionID("codex", "native"); err == nil || next == id {
		t.Fatal("corruption not refused")
	}
	// Corrupt migration state is likewise authoritative and cannot reset eligibility.
	if err := local.Write(filepath.Join(s.home, "identity-migration.json"), map[string]any{"complete": "wrong-type"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureAgentSessionID("codex", "fresh"); err == nil {
		t.Fatal("corrupt migration reset")
	}
}

func TestMigrationCannotOverwriteConcurrentEmptyStateCompletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	calls := 0
	complete, err := s.ReconcileIdentityIndexes(8, func() error {
		calls++
		if calls == 1 {
			// Model a direct allocator completing after reconciliation's initial
			// readiness/snapshot read but before its marker write. Both writes use
			// hooks.lock in production; the guard gives a deterministic interleaving.
			return s.ensureIdentityReady()
		}
		ready, err := s.identityReady()
		if err != nil {
			return err
		}
		if !ready {
			t.Error("completed migration rolled back before final save")
		}
		return nil
	})
	if err != nil || !complete {
		t.Fatal("completed migration lost", err)
	}
	if calls != 1 {
		t.Fatalf("enumerated already-completed state: %d guards", calls)
	}
	ready, err := s.identityReady()
	if err != nil || !ready {
		t.Fatal("completion not durable", err)
	}
}

func TestIdentityWriteFencesExistingNumericProtectedConfig(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// A previously guarded config had a numeric version and a skill marker
	// unknown to the published v0.1.1 reader. Keep its consent disabled.
	raw := []byte(`{"schema_version":2,"skill_evidence":"body+discovery-v2","discovery":{"enabled":false,"codex_homes":[],"authorizations":[]}}`)
	if err := os.WriteFile(filepath.Join(s.home, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureAgentSessionID("codex", "new-native"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var published struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &published); err == nil {
		t.Fatal("namespaced identity was written without fencing the published old reader")
	}
	cfg, found, err := config.Load(s.home)
	if err != nil || !found || cfg.Discovery == nil || cfg.Discovery.Enabled || cfg.EffectiveSkillEvidence() != config.SkillEvidenceBody {
		t.Fatal("fence migration changed existing consent or skill policy", err)
	}
}

// A competing hook can acquire hooks.lock before the migrator's final save.
// Empty state must already permit admission at that boundary, even while the
// separate migration lock remains held by the first hook.
func TestEmptyMigrationPermitsAdmissionBeforeFinalSave(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	releaseMigration, err := local.NamedLock(s.home, "identity-migration.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMigration()
	guarded := false
	complete, err := s.startIdentityMigration(identityMigration{}, func() error {
		guarded = true
		return config.ProtectIdentityWriter(s.home)
	})
	if err != nil || !complete || !guarded {
		t.Fatalf("empty migration left admission pending: complete=%t guarded=%t err=%v", complete, guarded, err)
	}
	releaseHooks, err := local.NamedLock(s.home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHooks()
	if _, _, err := s.EnsureAgentSessionID("cursor", "first-concurrent-start"); err != nil {
		t.Fatal("empty migration blocked competing admission", err)
	}
}
