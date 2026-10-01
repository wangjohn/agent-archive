package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func fixture(tb testing.TB) (*state.Store, config.Config, time.Time, string) {
	tb.Helper()
	canonical := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			tb.Fatal(err)
		}
		return resolved
	}
	home, project, codex := canonical(tb.TempDir()), canonical(tb.TempDir()), canonical(tb.TempDir())
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(home)
	if err != nil {
		tb.Fatal(err)
	}
	cfg := config.Config{MachineID: "synthetic-machine", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{codex}}}
	if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
		tb.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		tb.Fatal(err)
	}
	return store, cfg, at, codex
}

func writeRollout(tb testing.TB, codex, project string, at time.Time, n int, folder string) string {
	tb.Helper()
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
	path := filepath.Join(codex, folder, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		tb.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "synthetic", "cli_version": "test"}})
	task, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": at.Format(time.RFC3339Nano)}})
	prompt, _ := json.Marshal(map[string]any{"type": "response_item", "timestamp": at.Format(time.RFC3339Nano), "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "SYNTHETIC_BODY_ONLY"}}}})
	if err := os.WriteFile(path, []byte(string(meta)+"\n"+string(task)+"\n"+string(prompt)+"\n"), 0600); err != nil {
		tb.Fatal(err)
	}
	return id
}

func syntheticSupport(m sourcefacts.CodexMeta) bool {
	return m.Version == "test" && m.Originator == "synthetic"
}

func TestProductionScanKeepsUnverifiedNativeProducerUnregistered(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	h, err := Run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }})
	if err != nil || h.Registered != 0 || h.Supported || h.Outcomes["unsupported_producer"] != 1 {
		t.Fatalf("health=%#v err=%v", h, err)
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("unverified producer admitted")
	}
}

func TestSyntheticDiscoveryUsesFilteredPublicationAndPreservesHookEvidence(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatalf("health=%#v err=%v", h, err)
	}
	// Admission succeeded before a storage object exists: outage cannot prevent it.
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].Origin != archive.SessionOriginDiscovery {
		t.Fatal(regs)
	}
	objectStore := storagetest.NewMemoryStore()
	result, err := collector.Run(context.Background(), store, objectStore, collector.Options{MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	_, _, _, found, err := store.LoadPublished(regs[0].ArchiveSessionID)
	metadataBytes, _ := store.PublishedMetadata(regs[0].ArchiveSessionID)
	var meta archive.Metadata
	if decodeErr := json.Unmarshal(metadataBytes, &meta); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if err != nil || !found || meta.Origin != archive.SessionOriginDiscovery || meta.ImportedAt != nil {
		t.Fatalf("metadata=%#v err=%v", meta, err)
	}
	before := regs[0]
	payload := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": id, "cwd": before.ProjectRoot}
	if err := capture.HandleEvent(store.Home(), "codex", payload, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after, _, _ := store.LoadRegistration(before.ArchiveSessionID)
	if after.Origin != before.Origin || !after.AdmittedAt.Equal(before.AdmittedAt) || after.DestinationID != before.DestinationID || after.HookObservedAt.IsZero() {
		t.Fatal("hook erased discovery provenance")
	}
	raw, _ := os.ReadFile(filepath.Join(store.Home(), "discovery-catalog.json"))
	if strings.Contains(string(raw), "SYNTHETIC_BODY_ONLY") {
		t.Fatal("catalog retained conversation")
	}
	h, err = run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(5 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Probes != 0 {
		t.Fatalf("unchanged source repeated work: %#v %v", h, err)
	}
}

func TestBoundedScanContinuesFairlyAcrossActiveAndArchivedSources(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	for i := 1; i <= 600; i++ {
		writeRollout(t, root, project, at.Add(-time.Hour), i, "sessions")
	}
	writeRollout(t, root, project, at.Add(time.Minute), 9999, "archived_sessions")
	seen := 0
	completed := false
	for range 12 {
		h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
		if err != nil || h.Probes > HeaderProbes || h.Entries > 2200 {
			t.Fatalf("unbounded pass %#v %v", h, err)
		}
		seen += h.Probes
		if h.Registered == 1 {
			completed = true
		}
		if !h.Pending {
			break
		}
	}
	if !completed || seen < 601 {
		t.Fatalf("starved archived or old coverage: admitted=%t probes=%d", completed, seen)
	}
	var c catalog
	if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cache) > maxCatalog {
		t.Fatal("catalog unbounded")
	}
}

func TestDiscoveryCrashRecoveryAndRemovalNeverResurrect(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	h := sourcefacts.ReadHeader(context.Background(), root, filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl"))
	generation, _ := cfg.DiscoveryGeneration("codex", cfg.Archive.Projects[0].Root, h.Started, at.Add(2*time.Minute))
	reg, created, err := admit(store, candidateFromHeader(h, SourceDescriptor{Kind: archive.SourceKindFile, StableKey: h.Meta.ID, Root: root, Locator: filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")}), cfg.Archive.Projects[0].Root, generation, at.Add(2*time.Minute))
	if err != nil || !created {
		t.Fatal(err)
	}
	// Simulate crash after registration by removing its queued request.
	requests, _ := store.LoadRequests()
	if len(requests) > 0 {
		if _, err := store.CompleteRequest(reg.ArchiveSessionID, requests[0].Token); err != nil {
			t.Fatal(err)
		}
	}
	result, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("unrequested registration lost: %#v %v", result, err)
	}
	if err := store.RecordRemoval("codex", native, state.RemovalReasonUndo, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetSession(reg.ArchiveSessionID, native); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admit(store, candidateFromHeader(h, SourceDescriptor{Kind: archive.SourceKindFile, StableKey: h.Meta.ID, Root: root, Locator: reg.TranscriptPath}), reg.ProjectRoot, generation, at.Add(5*time.Minute)); err == nil {
		t.Fatal("removed session resurrected")
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("tombstone lost")
	}
}

func TestAdmissionRevalidatesPauseGenerationAndSkipsContendedHooks(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	h := sourcefacts.ReadHeader(context.Background(), root, path)
	g, _ := cfg.DiscoveryGeneration("codex", cfg.Archive.Projects[0].Root, h.Started, at.Add(2*time.Minute))
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := admit(store, candidateFromHeader(h, SourceDescriptor{Kind: archive.SourceKindFile, StableKey: h.Meta.ID, Root: root, Locator: path}), cfg.Archive.Projects[0].Root, g, at.Add(2*time.Minute)); err == nil {
		t.Fatal("admission ignored hooks lock")
	}
	unlock()
	if _, err := config.SetPausedAt(store.Home(), true, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admit(store, candidateFromHeader(h, SourceDescriptor{Kind: archive.SourceKindFile, StableKey: h.Meta.ID, Root: root, Locator: path}), cfg.Archive.Projects[0].Root, g, at.Add(3*time.Minute)); err == nil {
		t.Fatal("stale permission admitted")
	}
}

func BenchmarkCodexCatalog(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			store, cfg, at, root := fixture(b)
			for i := 1; i <= count; i++ {
				writeRollout(b, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), i, "sessions")
			}
			b.ResetTimer()
			probes, bytes, entries := 0, int64(0), 0
			for range b.N {
				h, err := Run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(time.Hour) }})
				if err != nil {
					b.Fatal(err)
				}
				probes += h.Probes
				bytes += h.Bytes
				entries += h.Entries
			}
			b.ReportMetric(float64(probes)/float64(b.N), "headers/pass")
			b.ReportMetric(float64(bytes)/float64(b.N), "read-bytes/pass")
			b.ReportMetric(float64(entries)/float64(b.N), "entries/pass")
		})
	}
}

type rejectedFieldCase string

const (
	rejectedFieldSource                      rejectedFieldCase = "source"
	rejectedFieldParentThreadId              rejectedFieldCase = "parent_thread_id"
	rejectedFieldHistoryBase                 rejectedFieldCase = "history_base"
	rejectedFieldForkedFromId                rejectedFieldCase = "forked_from_id"
	rejectedFieldSubagentHistoryStartOrdinal rejectedFieldCase = "subagent_history_start_ordinal"
	rejectedFieldSessionId                   rejectedFieldCase = "session_id"
	rejectedFieldCwd                         rejectedFieldCase = "cwd"
	rejectedFieldTimestamp                   rejectedFieldCase = "timestamp"
	rejectedFieldHistoryMode                 rejectedFieldCase = "history_mode"
	rejectedFieldOriginator                  rejectedFieldCase = "originator"
	rejectedFieldCliVersion                  rejectedFieldCase = "cli_version"
)

func TestRejectedMetadataPayloadNeverEntersCatalog(t *testing.T) {
	t.Parallel()
	for _, field := range []rejectedFieldCase{rejectedFieldSource, rejectedFieldParentThreadId, rejectedFieldHistoryBase, rejectedFieldForkedFromId, rejectedFieldSubagentHistoryStartOrdinal, rejectedFieldSessionId, rejectedFieldCwd, rejectedFieldTimestamp, rejectedFieldHistoryMode, rejectedFieldOriginator, rejectedFieldCliVersion} {
		t.Run(string(field), func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
			raw, _ := os.ReadFile(path)
			lines := strings.Split(string(raw), "\n")
			var first map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
				t.Fatal(err)
			}
			payload := first["payload"].(map[string]any)
			secret := "PRIVATE_BODY_MUST_NEVER_ENTER_CATALOG"
			if field == rejectedFieldSource || field == rejectedFieldParentThreadId || field == rejectedFieldHistoryBase || field == rejectedFieldForkedFromId || field == rejectedFieldSubagentHistoryStartOrdinal {
				payload[string(field)] = map[string]any{"prompt": secret}
			} else {
				payload[string(field)] = strings.Repeat(secret, 100)
			}
			encoded, _ := json.Marshal(first)
			lines[0] = string(encoded)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}); err != nil {
				t.Fatal(err)
			}
			catalogBytes, _ := os.ReadFile(filepath.Join(store.Home(), "discovery-catalog.json"))
			if strings.Contains(string(catalogBytes), secret) {
				t.Fatalf("%s retained rejected payload", field)
			}
		})
	}
}

func TestDiscoveryRelocatesValidatedContinuationWithoutNewGeneration(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	id := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatal("initial admission", err)
	}
	regs, _ := store.LoadRegistrations()
	before := regs[0]
	archived := filepath.Join(root, "archived_sessions", filepath.Base(before.TranscriptPath))
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(before.TranscriptPath, archived); err != nil {
		t.Fatal(err)
	}
	// Change the discovery generation after the original task: continuation is
	// the existing registration, never a new start-authorized import.
	previous := cfg
	d := *cfg.Discovery
	cfg.Discovery = &d
	cfg.Discovery.CodexHomes = append(cfg.Discovery.CodexHomes, t.TempDir())
	if err := config.ReconcileDiscovery(&cfg, previous, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, err = run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(4 * time.Minute) }}, syntheticSupport)
		if err != nil {
			t.Fatal(err)
		}
	}
	archiveID, _, _ := store.AgentSessionID("codex", id)
	after, _, _ := store.LoadRegistration(archiveID)
	if after.TranscriptPath != archived || after.ArchiveSessionID != before.ArchiveSessionID || !after.AdmittedAt.Equal(before.AdmittedAt) || after.DiscoveryGeneration != before.DiscoveryGeneration {
		t.Fatal("continuation lost original facts")
	}
	// A validated active copy is preferred when both locations exist.
	writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	for range 3 {
		_, err = run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(5 * time.Minute) }}, syntheticSupport)
		if err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ = store.LoadRegistration(archiveID)
	if after.TranscriptPath != before.TranscriptPath {
		t.Fatal("active source not preferred")
	}
}

func TestNativeDateHintFindsFreshTaskAheadOfColdHistory(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	for i := 1; i <= 1200; i++ {
		writeRollout(t, root, project, at.Add(-time.Hour), i, "sessions/2026/09/30")
	}
	writeRollout(t, root, project, at.Add(time.Minute), 9000, "sessions/2026/10/01")
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 || h.Probes > HeaderProbes || !h.Pending {
		t.Fatalf("priority missed or erased backlog: %#v %v", h, err)
	}
	// A date-shaped location cannot authorize an old start or an unknown producer.
	writeRollout(t, root, project, at.Add(-time.Hour), 9001, "sessions/2026/10/01")
	h, err = run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(3 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 0 {
		t.Fatal("date hint became eligibility", err)
	}
}

// Lifecycle hooks cannot switch a discovery source without the source facts
// used for discovery's confined continuation checks.
func TestHookContinuationPreservesValidatedDiscoveryLocator(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"outside_root", "stale_archived"} {
		t.Run(location, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			native := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
			h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			if err != nil || h.Registered != 1 {
				t.Fatalf("initial discovery: %#v %v", h, err)
			}
			regs, _ := store.LoadRegistrations()
			before := regs[0]
			misleading := filepath.Join(t.TempDir(), filepath.Base(before.TranscriptPath))
			if location == "stale_archived" {
				misleading = filepath.Join(root, "archived_sessions", filepath.Base(before.TranscriptPath))
			}
			if err := os.MkdirAll(filepath.Dir(misleading), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(misleading, []byte("unrelated or stale source\n"), 0600); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": native, "cwd": project, "transcript_path": misleading}
			hookAt := at.Add(3 * time.Minute)
			if err := capture.HandleEvent(store.Home(), "codex", payload, hookAt); err != nil {
				t.Fatal(err)
			}
			after, found, err := store.LoadRegistration(before.ArchiveSessionID)
			if err != nil || !found || after.TranscriptPath != before.TranscriptPath || after.DiscoveryRoot != before.DiscoveryRoot || after.DiscoveryCwd != before.DiscoveryCwd || after.DiscoveryGeneration != before.DiscoveryGeneration || after.Origin != before.Origin || after.DestinationID != before.DestinationID || !after.SessionStartedAt.Equal(before.SessionStartedAt) || !after.AdmittedAt.Equal(before.AdmittedAt) || !after.HookObservedAt.Equal(hookAt) {
				t.Fatalf("hook replaced discovery facts: %#v %v", after, err)
			}
			result, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(4 * time.Minute) }})
			if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatalf("hook broke filtered publication: %#v %v", result, err)
			}
		})
	}
}
