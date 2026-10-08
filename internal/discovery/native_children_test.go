package discovery

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeChildAdmissionUsesOwnTaskBeyondHeaderWindowWithoutParent(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "own_task", true: "invalid_first_own"}[invalid], func(t *testing.T) {
			store, cfg, at, home := fixture(t)
			parent := "00000000-0000-0000-0000-000000000900"
			id, path := compatibilityRollout(t, home, cfg.Archive.Projects[0].Root, "codex-155-legacy.jsonl", "dev", 901, func(m, task map[string]any) {
				m["session_id"], m["parent_thread_id"] = parent, parent
				m["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
				m["subagent_history_start_ordinal"] = 1025
				if invalid {
					task["turn_id"] = "external-import-turn"
				}
			})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			var builder strings.Builder
			builder.WriteString(lines[0] + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + parent + `","started_at":"2026-10-01T12:01:00Z"}}` + "\n")
			for range 1023 {
				builder.WriteString(`{"type":"turn_context","payload":{"model":"synthetic inherited model","synthetic_padding":"` + strings.Repeat("x", 512) + `"}}` + "\n")
			}
			firstOwnByte := builder.Len()
			if firstOwnByte <= sourcefacts.HeaderBytes {
				t.Fatalf("fixture does not exceed header byte window: %d", firstOwnByte)
			}
			t.Logf("first own task starts after %d bytes and 1025 raw records; header limits %d bytes/%d records", firstOwnByte, sourcefacts.HeaderBytes, sourcefacts.HeaderRecords)
			builder.WriteString(strings.Join(lines[1:], "\n"))
			if invalid {
				builder.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `","started_at":"2026-10-01T12:01:00Z"}}` + "\n")
			}
			if err := os.WriteFile(path, []byte(builder.String()), 0600); err != nil {
				t.Fatal(err)
			}
			h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
			if err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if invalid {
				if len(regs) != 0 || h.Outcomes["own_task_rejected"] == 0 {
					t.Fatalf("copied/later tasks licensed child: %+v %v", h, regs)
				}
				for range 3 {
					warm, err := runWithCensus(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Hour) }}, registeredAdapters())
					if err != nil {
						t.Fatal(err)
					}
					if warm.NativeValidationAttempts != 0 || warm.NativeValidationBytes != 0 {
						t.Fatal("decisively rejected first task reread unchanged source", warm.NativeValidationAttempts, warm.NativeValidationBytes)
					}
				}
				// A source rewrite must reopen the decision, while unrelated
				// work remains admissible despite the terminal negative.
				writeRollout(t, home, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 902, "sessions")
				fresh, err := runWithCensus(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(4 * time.Minute) }}, registeredAdapters())
				if err != nil || fresh.Registered != 1 {
					t.Fatal("terminal negative pinned unrelated work", fresh, err)
				}
				changed := strings.Replace(builder.String(), `"turn_id":"external-import-turn"`, `"turn_id":"`+id+`"`, 1)
				// Preserve size and modification time while replacing the inode:
				// a terminal decision must bind the exact live source identity.
				prior, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				changed = strings.Replace(changed, strings.Repeat("x", 512), strings.Repeat("x", 512-(len(changed)-builder.Len())), 1)
				if len(changed) != builder.Len() {
					t.Fatal("replacement fixture changed source size")
				}
				replacement := filepath.Join(filepath.Dir(path), "replacement.tmp")
				if err := os.WriteFile(replacement, []byte(changed), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(replacement, prior.ModTime(), prior.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
				repaired, err := runWithCensus(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(5 * time.Minute) }}, registeredAdapters())
				if err != nil || repaired.Registered != 1 {
					t.Fatal("changed rejected source stayed terminal", repaired, err)
				}
				return
			}
			if len(regs) != 1 || h.Registered != 1 {
				t.Fatalf("independent child remained pending: %+v %v", h, regs)
			}
			t.Logf("initial native proof: opens=%d reads=%d actual native bytes=%d probes=%d", h.NativeValidationOpens, h.NativeValidationReads, h.NativeValidationBytes, h.Probes)
			reg := regs[0]
			if reg.NativeSessionID != id || !reg.NativeChild || reg.ParentNativeSessionID != parent || reg.ParentSessionID != "" || reg.Origin != archive.SessionOriginDiscovery || !reg.HookObservedAt.IsZero() || !reg.ImportBatch.IsZero() || reg.CodexBinding == nil || reg.CodexBinding.OwnStart == nil || *reg.CodexBinding.OwnStart != 1025 {
				t.Fatalf("native provenance lost: %+v", reg)
			}
			saved, _, err := config.Load(store.Home())
			if err != nil || !saved.CodexHistoryProtection {
				t.Fatal("writer fence missing", err)
			}
			encoded, _ := json.Marshal(h)
			if strings.Contains(string(encoded), parent) || strings.Contains(string(encoded), path) {
				t.Fatal("private native facts leaked in aggregate health")
			}
		})
	}
}

func TestMissingOwnTaskRetriesUntilCompleteSourceArrives(t *testing.T) {
	t.Parallel()
	store, cfg, at, home := fixture(t)
	parent := "00000000-0000-0000-0000-000000000900"
	id, path := compatibilityRollout(t, home, cfg.Archive.Projects[0].Root, "codex-155-legacy.jsonl", "dev", 903, func(meta, task map[string]any) {
		meta["parent_thread_id"] = parent
		meta["session_id"] = parent
		meta["subagent_history_start_ordinal"] = 65
	})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(original), "\n")
	var prefix strings.Builder
	prefix.WriteString(lines[0] + "\n")
	for range 64 {
		prefix.WriteString(`{"type":"turn_context","payload":{"model":"synthetic copied"}}` + "\n")
	}
	// A trailing incomplete task is outside the validated complete-record prefix.
	partial := prefix.String() + `{"type":"event_msg","payload":{"type":"task_started"`
	if err := os.WriteFile(path, []byte(partial), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		h, err := runWithCensus(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
		if err != nil || h.Registered != 0 || h.Outcomes["own_task_rejected"] != 0 || !h.Pending {
			t.Fatal("partial missing task became terminal", h, err)
		}
	}
	complete := prefix.String() + strings.Join(lines[1:], "\n")
	if err := os.WriteFile(path, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := runWithCensus(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(3 * time.Minute) }}, registeredAdapters())
	if err != nil || h.Registered != 1 {
		t.Fatal("complete own task did not retry", id, h, err)
	}
}
