package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestCodexBindingMigrationPreservesAllRegistrationOrigins(t *testing.T) {
	for _, origin := range []archive.SessionOrigin{archive.SessionOriginHook, archive.SessionOriginImport, archive.SessionOriginDiscovery} {
		t.Run(string(origin), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, filepath.Join(t.TempDir(), "old.jsonl"))
			reg.NativeSessionID = "11111111-1111-4111-8111-111111111111"
			reg.Origin = origin
			reg.AdmittedAt = reg.RegisteredAt
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{MachineID: "machine", Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: reg.ProjectRoot, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
			if err := config.Save(local.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			facts := &archive.CodexSourceBinding{Version: 1, NativeThreadID: reg.NativeSessionID, NativeCreatedAt: reg.SessionStartedAt, Cwd: reg.ProjectRoot, ProducerSource: "cli", PhysicalRolloutID: reg.NativeSessionID, Path: filepath.Join(t.TempDir(), "current.jsonl")}
			scan := &sessionScan{local: local, reg: reg}
			if err := scan.persistCodexBinding(facts); err != nil {
				t.Fatal(err)
			}
			current, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatalf("load migration: %v", err)
			}
			current.CodexBinding = nil
			current.TranscriptPath = reg.TranscriptPath
			before, _ := json.Marshal(reg)
			after, _ := json.Marshal(current)
			if string(before) != string(after) {
				t.Fatalf("migration changed provenance: %s / %s", before, after)
			}
			cfg, _, err = config.Load(local.Home())
			if err != nil || !cfg.CodexHistoryProtection {
				t.Fatalf("missing permanent writer guard: %v", err)
			}
			path := filepath.Join(local.Home(), "registrations", reg.ArchiveSessionID+".json")
			stamp, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := scan.persistCodexBinding(facts); err != nil {
				t.Fatal(err)
			}
			again, err := os.Stat(path)
			if err != nil || !again.ModTime().Equal(stamp.ModTime()) {
				t.Fatalf("settled binding was rewritten: %v", err)
			}
			_, err = local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.CodexBinding.Cwd = "/other"; return nil })
			if err == nil {
				t.Fatal("in-place immutable binding mutation accepted")
			}
		})
	}
}
