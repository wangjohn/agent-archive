package rolloutcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestProviderUsesSharedCatalogForCrossHomeLineage(t *testing.T) {
	for _, scenario := range []string{"connected", "rewrite_after_open", "missing_base", "disconnected", "outside_root"} {
		t.Run(scenario, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			body := `{"type":"response_item","ordinal":1,"payload":{"type":"message","role":"user","content":"synthetic"}}` + "\n"
			base := fixture(t, a, "archived_sessions/deep", thread, thread, nil, body)
			raw, err := os.ReadFile(base)
			if err != nil {
				t.Fatal(err)
			}
			baseID := thread
			if scenario == "missing_base" {
				baseID = "33333333-3333-4333-8333-333333333333"
			}
			leaf := fixture(t, b, "sessions", revision, thread, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: baseID, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "")
			bytes, err := os.ReadFile(leaf)
			if err != nil {
				t.Fatal(err)
			}
			var frame map[string]any
			if err := json.Unmarshal(bytes, &frame); err != nil {
				t.Fatal(err)
			}
			frame["ordinal"] = 2
			bytes, err = json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			bytes = append(bytes, '\n')
			bytes = append(bytes, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":"synthetic own"}}`+"\n")...)
			if err := os.WriteFile(leaf, bytes, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "disconnected" {
				fixture(t, b, "sessions", "33333333-3333-4333-8333-333333333333", thread, nil, "")
			}
			homes := []string{a, b}
			if scenario == "outside_root" {
				homes = []string{b}
			}
			c := New(homes, Limits{})
			canonicalHome, err := filepath.EvalSymlinks(b)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer bound.Close()
			before := c.Counters()
			pass, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: sourcefacts.RootOpener{Root: canonicalHome}, Policy: transcriptio.OpenPolicy{Root: canonicalHome, RejectSymlinks: true}, CodexRollouts: bound})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := pass.Close(); err != nil {
					t.Error(err)
				}
			}()
			snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: leaf}, agentapi.ReadLimits{})
			if scenario != "connected" && scenario != "rewrite_after_open" {
				if err == nil {
					_ = snapshot.Close()
					t.Fatal("unproved lineage read")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := snapshot.Close(); err != nil {
					t.Error(err)
				}
			}()
			if scenario == "rewrite_after_open" {
				if err := os.WriteFile(base, []byte(strings.Replace(string(raw), "synthetic", "rewritten", 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input := snapshot.Input().Records
			if input == nil {
				t.Fatal("history record input missing")
			}
			count := 0
			for {
				_, ok, err := input.Next(t.Context())
				if err != nil {
					if scenario == "rewrite_after_open" && agentapi.Failure(err) == agentapi.Changed {
						return
					}
					t.Fatal(err)
				}
				if !ok {
					break
				}
				count++
			}
			if scenario == "rewrite_after_open" {
				t.Fatal("cached slice bypassed selected-handle prefix checks")
			}
			if count != 5 {
				t.Fatalf("history frames: %d", count)
			}
			counters := c.Counters()
			if counters.Headers != 2 || counters.Directories != 3 || counters.PrefixBytes != 0 || counters.ValidationSweeps != 1 || counters.FileBytes <= before.FileBytes || counters.FileOpens <= before.FileOpens {
				t.Fatalf("unexpected shared epoch cost %#v", counters)
			}
		})
	}
}

func TestCatalogDependencyAuthorityDoesNotBroadenSeedPolicy(t *testing.T) {
	t.Parallel()
	approved, seedHome := t.TempDir(), t.TempDir()
	fixture(t, approved, "sessions", thread, thread, nil, "")
	seed := fixture(t, seedHome, "sessions", revision, thread, nil, "")
	root, err := filepath.EvalSymlinks(approved)
	if err != nil {
		t.Fatal(err)
	}
	c := New([]string{approved, seedHome}, Limits{})
	pass, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{
		Files: sourcefacts.RootOpener{Root: root}, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}, CodexRollouts: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	if snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: seed}, agentapi.ReadLimits{}); err == nil {
		_ = snapshot.Close()
		t.Fatal("catalog authority bypassed original seed policy")
	}
	if c.Counters().Headers != 0 {
		t.Fatal("rejected seed enumerated dependency inventory")
	}
}
