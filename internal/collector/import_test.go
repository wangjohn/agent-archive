package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func publishedMetadataBytes(t *testing.T, reg archive.SessionRegistration) []byte {
	t.Helper()
	local := newTestStore(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, Options{MachineID: "machine", Now: func() time.Time { return now }}); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := remote.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A hook registration now carries AdmittedAt, Origin, and StartedAtSource.
// None of them may reach its metadata: it must be byte-identical to what a
// registration without them published before.
func TestHookMetadataIsByteIdenticalWithAdmissionFields(t *testing.T) {
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	legacy := registration(t, path)
	hook := legacy
	hook.AdmittedAt = hook.SessionStartedAt
	hook.Origin = archive.SessionOriginHook
	hook.StartedAtSource = archive.StartedAtSourceHook

	before, after := publishedMetadataBytes(t, legacy), publishedMetadataBytes(t, hook)
	if !bytes.Equal(before, after) {
		t.Fatalf("hook metadata changed:\nbefore %s\nafter  %s", before, after)
	}
	for _, key := range []string{`"origin"`, `"imported_at"`, `"started_at_source"`, archive.CaptureGapImportedWithoutHookEvidence} {
		if bytes.Contains(after, []byte(key)) {
			t.Fatalf("hook metadata carries %s: %s", key, after)
		}
	}
}

// An imported session's metadata says so, keeps its true start, and records
// the evidence it lacks, on first publication and after a metadata-only
// parser upgrade alike.
func TestImportedSessionMetadataRecordsProvenanceAndGap(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	importedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	reg.AdmittedAt, reg.RegisteredAt = importedAt, importedAt
	reg.Origin, reg.StartedAtSource, reg.ImportBatch = archive.SessionOriginImport, archive.StartedAtSourceTranscript, "2026-09-23-1"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	now := importedAt.Add(time.Minute)
	opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	check := func(label string) {
		t.Helper()
		metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
		if metadata.Origin != archive.SessionOriginImport || metadata.ImportedAt == nil || !metadata.ImportedAt.Equal(importedAt) || metadata.StartedAtSource != archive.StartedAtSourceTranscript {
			t.Fatalf("%s: provenance origin=%q imported_at=%v source=%q", label, metadata.Origin, metadata.ImportedAt, metadata.StartedAtSource)
		}
		if !metadata.StartedAt.Equal(reg.SessionStartedAt) {
			t.Fatalf("%s: started_at=%v, want the true start %v", label, metadata.StartedAt, reg.SessionStartedAt)
		}
		gaps := 0
		for _, gap := range metadata.CaptureGaps {
			if gap.Code == archive.CaptureGapImportedWithoutHookEvidence {
				gaps++
			}
		}
		if gaps != 1 {
			t.Fatalf("%s: capture gaps=%#v", label, metadata.CaptureGaps)
		}
	}
	check("first publication")

	opts.ParserVersion = "two"
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); metadata.Parser.Version != "two" {
		t.Fatalf("metadata was not regenerated: %+v", metadata.Parser)
	}
	check("parser upgrade")
}

// A subagent is admitted with its parent: it copies the parent's AdmittedAt,
// Origin, ImportBatch, and DestinationID. Only a subagent a SubagentStop hook reported gets
// that hook's lifecycle event; one backfill found gets none. A hook that
// reports a subagent of a resumed import gives an import child the event.
func TestSubagentInheritsAdmissionAndOnlyHookChildrenGetLifecycleEvidence(t *testing.T) {
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	importedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		origin          archive.SessionOrigin
		admittedAt      time.Time
		batch           string
		observedAt      time.Time
		candidateOrigin archive.SessionOrigin
		wantLifecycle   int
	}{
		{"hook", archive.SessionOriginHook, parentStart, "", parentStart.Add(3 * time.Minute), archive.SessionOriginHook, 1},
		{"import", archive.SessionOriginImport, importedAt, "2026-09-23-1", importedAt, archive.SessionOriginImport, 0},
		{"hook-reported child of an import", archive.SessionOriginImport, importedAt, "2026-09-23-1", importedAt.Add(time.Hour), archive.SessionOriginHook, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			local, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			parentPath := filepath.Join(home, "parent.jsonl")
			childPath := filepath.Join(home, "child.jsonl")
			if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(childPath, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			parent := archive.SessionRegistration{
				ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project",
				Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: tc.admittedAt,
				AdmittedAt: tc.admittedAt, Origin: tc.origin, ImportBatch: tc.batch,
				// The destination configured below: a zero storage.
				DestinationID: config.Config{}.DestinationID(),
			}
			if tc.origin == archive.SessionOriginImport {
				parent.StartedAtSource = archive.StartedAtSourceTranscript
			}
			if err := local.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveSubagentCandidate(state.SubagentCandidate{
				ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native",
				ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: childPath,
				ObservedAt: tc.observedAt, Origin: tc.candidateOrigin,
			}); err != nil {
				t.Fatal(err)
			}
			// The project was activated when the parent was admitted, so the
			// child's own start (10:02) predates activation for an import; it
			// is accepted only through the admission it inherits.
			cfg := config.Config{Harnesses: []string{"claude"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{
				ProjectID: "project", Root: "/project", Included: true, ActivatedAt: tc.admittedAt,
			}}}}
			remote := storage.NewMemoryStore()
			now := tc.observedAt.Add(time.Minute)
			result, err := Run(context.Background(), local, remote, Options{MachineID: "machine", Now: func() time.Time { return now }, AcceptSession: cfg.AcceptSession})
			if err != nil || len(result.Errors) != 0 {
				t.Fatalf("%#v %v", result, err)
			}
			child, found, err := local.LoadRegistration("child")
			if err != nil || !found {
				t.Fatalf("child not registered: found=%v err=%v", found, err)
			}
			if !child.AdmittedAt.Equal(parent.AdmittedAt) || child.Origin != parent.Origin || child.ImportBatch != parent.ImportBatch || child.DestinationID != parent.DestinationID {
				t.Fatalf("child did not inherit admission: %#v", child)
			}
			if !child.SessionStartedAt.Equal(parentStart.Add(2 * time.Minute)) {
				t.Fatalf("child start=%v, want its native start", child.SessionStartedAt)
			}
			bundle, _, published, err := local.LoadLastPublished("child")
			if err != nil || !published {
				t.Fatalf("child not published: %v", err)
			}
			lifecycle := 0
			for _, evidence := range bundle.SupplementalEvidence {
				if evidence.Kind == archive.EvidenceKindLifecycleHook {
					lifecycle++
				}
			}
			if lifecycle != tc.wantLifecycle {
				t.Fatalf("lifecycle evidence=%d, want %d", lifecycle, tc.wantLifecycle)
			}
			metadata := fetchMetadata(t, remote, "claude", "child")
			imported := tc.origin == archive.SessionOriginImport
			if (metadata.Origin == archive.SessionOriginImport) != imported {
				t.Fatalf("child metadata origin=%q", metadata.Origin)
			}
			// The child's start is its earliest native record.
			if wantSource := map[bool]archive.StartedAtSource{true: archive.StartedAtSourceTranscript}[imported]; metadata.StartedAtSource != wantSource || child.StartedAtSource != wantSource {
				t.Fatalf("started_at_source metadata=%q registration=%q, want %q", metadata.StartedAtSource, child.StartedAtSource, wantSource)
			}
			gap := false
			for _, g := range metadata.CaptureGaps {
				if g.Code == archive.CaptureGapImportedWithoutHookEvidence {
					gap = strings.Contains(g.Detail, "before it was imported")
				}
			}
			if gap != imported {
				t.Fatalf("capture gaps=%#v", metadata.CaptureGaps)
			}
		})
	}
}

func TestRemovalRecordRoundTripWithoutNativeID(t *testing.T) {
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if _, found, err := local.Removal("codex", "native-secret"); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if err := local.RecordRemoval("codex", "native-secret", state.RemovalReasonRetention, at); err != nil {
		t.Fatal(err)
	}
	record, found, err := local.Removal("codex", "native-secret")
	if err != nil || !found || record.Harness != "codex" || record.Reason != state.RemovalReasonRetention || !record.At.Equal(at) {
		t.Fatalf("record=%#v found=%v err=%v", record, found, err)
	}
	// The app is part of the key: the same native ID from another app is a
	// different session.
	if _, found, _ := local.Removal("claude", "native-secret"); found {
		t.Fatal("removal record matched another app")
	}
	if err := local.RecordRemoval("codex", "native-secret", state.RemovalReasonUndo, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := local.Removal("codex", "native-secret"); record.Reason != state.RemovalReasonUndo {
		t.Fatalf("later removal did not replace the record: %#v", record)
	}
	if err := local.RecordRemoval("codex", "native-secret", "expired", at); err == nil {
		t.Fatal("unknown reason accepted")
	}
	entries, err := os.ReadDir(filepath.Join(home, "forgotten"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "forgotten", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "native-secret") || strings.Contains(entries[0].Name(), "native-secret") {
		t.Fatalf("removal record exposes the native ID: %s %s", entries[0].Name(), data)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 3 || fields["app"] != "codex" || fields["reason"] != "undo" || fields["at"] == nil {
		t.Fatalf("record=%s err=%v", data, err)
	}
}

// A registration written under the "claude-code" spelling (a hand-edited
// hook) leaves a record backfill finds under "claude".
func TestRemovalRecordCanonicalApp(t *testing.T) {
	local, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.RecordRemoval("claude-code", "native-secret", state.RemovalReasonRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := local.Removal("claude", "native-secret"); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

// The removal record is written under the request lock, after the recheck for
// new work and before the session is forgotten. A session a hook kept alive
// gets no record; a record that cannot be written leaves the session
// registered, and a retry then records and forgets it.
func TestForgetIdleSessionRecordsRemovalOnlyWhenItForgets(t *testing.T) {
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	removal := &state.RemovalRecord{Harness: "codex", Reason: state.RemovalReasonUndo, At: at}

	kept := newTestStore(t)
	reg := registration(t, "/unused")
	if err := kept.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := kept.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := kept.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, true, removal); err != nil || forgotten {
		t.Fatalf("forgotten=%t err=%v", forgotten, err)
	}
	if _, found, err := kept.Removal("codex", reg.NativeSessionID); err != nil || found {
		t.Fatalf("a kept session has a removal record: found=%t err=%v", found, err)
	}

	home := t.TempDir()
	failing, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := failing.EnsureArchiveSessionID(reg.NativeSessionID); err != nil {
		t.Fatal(err)
	}
	// A file where the records directory belongs makes every write fail.
	blocker := filepath.Join(home, "forgotten")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := failing.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, true, removal); err == nil || forgotten {
		t.Fatalf("forgotten=%t err=%v, want a failure", forgotten, err)
	}
	if _, registered, _ := failing.LoadRegistration(reg.ArchiveSessionID); !registered {
		t.Fatal("the session was forgotten without its removal record")
	}
	if _, indexed, _ := failing.ArchiveSessionID(reg.NativeSessionID); !indexed {
		t.Fatal("the native index entry was removed without a removal record")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := failing.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, true, removal); err != nil || !forgotten {
		t.Fatalf("retry: forgotten=%t err=%v", forgotten, err)
	}
	if record, found, err := failing.Removal("codex", reg.NativeSessionID); err != nil || !found || record.Reason != state.RemovalReasonUndo || !record.At.Equal(at) {
		t.Fatalf("record=%#v found=%t err=%v", record, found, err)
	}
}

// An empty subagent transcript a hook reported may still be written, so its
// candidate waits. One backfill found is history and will not grow: it is
// rejected once, and the parent is told, instead of being retried forever.
func TestEmptyImportedSubagentIsRejectedAndHookOneWaits(t *testing.T) {
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	importedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		origin     archive.SessionOrigin
		wantWaits  bool
		wantReason string
	}{
		{"hook", archive.SessionOriginHook, true, ""},
		{"import", archive.SessionOriginImport, false, "subagent_transcript_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			local, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			parentPath := filepath.Join(home, "parent.jsonl")
			childPath := filepath.Join(home, "child.jsonl")
			if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(childPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			parent := archive.SessionRegistration{
				ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project",
				Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: importedAt,
				AdmittedAt: importedAt, Origin: tc.origin,
			}
			if tc.origin == archive.SessionOriginImport {
				parent.StartedAtSource, parent.ImportBatch = archive.StartedAtSourceTranscript, "2026-09-23-1"
			}
			if err := local.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveSubagentCandidate(state.SubagentCandidate{
				ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native",
				ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: childPath,
				ObservedAt: importedAt, Origin: tc.origin,
			}); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				materializeSubagentCandidates(local, Options{})
			}
			candidates, err := local.LoadSubagentCandidates()
			if err != nil {
				t.Fatal(err)
			}
			if waits := len(candidates) == 1; waits != tc.wantWaits {
				t.Fatalf("candidate still waiting=%t, want %t", waits, tc.wantWaits)
			}
			if _, found, _ := local.LoadRegistration("child"); found {
				t.Fatal("an empty subagent transcript was registered")
			}
			requests, err := local.LoadRequests()
			if err != nil {
				t.Fatal(err)
			}
			reason := ""
			for _, req := range requests {
				if req.ArchiveSessionID == "parent" {
					reason = strings.Join(req.Reasons, ",")
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("parent request reasons=%q, want %q", reason, tc.wantReason)
			}
		})
	}
}
