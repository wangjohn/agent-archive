package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
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

func TestRunUnknownHomeMigrationUsesConfiguredConfinementAndPreservesAdmission(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	root := scan.reg.ProjectRoot
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for id, refs := range lookup.refs {
		for i := range refs {
			old := refs[i].Path
			refs[i].Path = filepath.Join(root, "sessions", filepath.Base(old))
			if err := os.Rename(old, refs[i].Path); err != nil {
				t.Fatal(err)
			}
		}
		lookup.refs[id] = refs
	}
	lookup.set.Candidates = nil
	for _, id := range []string{revisionThread, revisionB, revisionC} {
		lookup.set.Candidates = append(lookup.set.Candidates, lookup.refs[id][0])
	}
	current := lookup.refs[revisionC][0]
	lookup.set.Current = &current
	scan.reg.TranscriptPath = lookup.refs[revisionThread][0].Path
	scan.reg.CodexBinding.Path = scan.reg.TranscriptPath
	scan.reg.CodexBinding.Home = ""
	if err := scan.local.SaveRegistration(scan.reg); err != nil {
		t.Fatal(err)
	}
	// Locator hints alone cannot read a related graph or confer home ownership.
	result, err := Run(t.Context(), scan.local, scan.remote, scan.opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("lookup hints supplied source permission", result, err)
	}
	before := scan.reg
	cfg, _, err := config.Load(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Discovery = &config.DiscoveryConfig{Enabled: false, CodexHomes: []string{root}}
	if err := config.Save(scan.local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	opts := scan.opts
	opts.ConfiguredCodexHomes = []string{root}
	opts.RepoKey = func(string) string { return "" }
	for range 4 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				var source *agentapi.SourceError
				errors.As(err, &source)
				t.Fatal(result.Errors, source.Err)
			}
		}
	}
	after, found, err := scan.local.LoadRegistration(scan.id())
	if err != nil || !found || after.CodexBinding.Home != root {
		t.Fatal("actual configured home not migrated", err)
	}
	after.CodexBinding = before.CodexBinding
	after.TranscriptPath = before.TranscriptPath
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("home migration changed admission/provenance")
	}
	path := filepath.Join(scan.local.Home(), "registrations", scan.id()+".json")
	stamp, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), scan.local, scan.remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	again, err := os.Stat(path)
	if err != nil || !again.ModTime().Equal(stamp.ModTime()) {
		t.Fatal("settled migration rewrote registration", err)
	}
}
