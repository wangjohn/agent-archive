package rolloutcatalog

import (
	"encoding/json"
	"fmt"
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

func TestProviderSharesAncestorWithinSeveralValidationSlices(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	base := fixture(t, home, "archived_sessions", thread, thread, nil, "{\"ordinal\":1}\n")
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	const children = 24
	paths := make([]string, 0, children)
	for i := range children {
		id := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1)
		path := fixture(t, home, "sessions", id, id, map[string]any{
			"parent_thread_id": thread,
			"history_base":     codexmeta.CodexHistoryPosition{RolloutID: thread, EndOrdinal: 2, EndByteOffset: uint64(len(raw))},
		}, "")
		bytes, err := os.ReadFile(path)
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
		if err := os.WriteFile(path, append(bytes, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	c := New([]string{home}, Limits{})
	for start := 0; start < children; start += 8 {
		bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
		if err != nil {
			t.Fatal(err)
		}
		pass, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: bound})
		if err != nil {
			_ = bound.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := pass.Close(); err != nil {
				t.Error(err)
			}
			if err := bound.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, path := range paths[start : start+8] {
			snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
			if err != nil {
				_ = pass.Close()
				_ = bound.Close()
				t.Fatal(err)
			}
			for {
				_, ok, err := snapshot.Input().Records.Next(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
			}
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
		}
		// Every snapshot closes before either the provider pass or slice renews.
		if err := pass.Close(); err != nil {
			t.Fatal(err)
		}
		if err := bound.Close(); err != nil {
			t.Fatal(err)
		}
	}
	counts := c.Counters()
	// Seeds use the caller opener; only one shared ancestor is catalog-opened
	// per slice, despite eight children each reading and verifying its prefix.
	if counts.Headers != children+1 || counts.ValidationSweeps != 3 || counts.FileOpens != children+1+2+3 || counts.PrefixBytes != 0 || counts.CheckOperations > 3*(children+10) {
		t.Fatalf("shared dependency work repeated: %#v", counts)
	}
}
