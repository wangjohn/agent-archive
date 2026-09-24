package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// deleteRecordingStore is the memory bucket, recording the order of deletes. A
// delete of a key containing failOn fails.
type deleteRecordingStore struct {
	*storage.MemoryStore
	mu      sync.Mutex
	deletes []string
	failOn  string
}

func (s *deleteRecordingStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn != "" && strings.Contains(key, s.failOn) {
		return errors.New("simulated outage")
	}
	s.deletes = append(s.deletes, key)
	return s.MemoryStore.Delete(ctx, key)
}

func (s *deleteRecordingStore) reset(failOn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes, s.failOn = nil, failOn
}

// newUndoFixture is an import fixture whose bucket records deletes, with the
// default import already run and uploaded.
func newUndoFixture(t *testing.T, importArgs ...string) (*backfillFixture, *deleteRecordingStore) {
	t.Helper()
	f, memory := newImportFixture(t)
	bucket := &deleteRecordingStore{MemoryStore: memory}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	if _, errOut, code := f.importRun(t, nil, false, append([]string{"--yes"}, importArgs...)...); code != 0 {
		t.Fatalf("import: code %d, %s", code, errOut)
	}
	bucket.reset("")
	return f, bucket
}

func (f *backfillFixture) undoRun(t *testing.T, stdin io.Reader, terminal bool, args ...string) (string, string, int) {
	t.Helper()
	return f.importRun(t, stdin, terminal, append([]string{"undo"}, args...)...)
}

// sessionKeys lists the bucket's objects under a session's prefix.
func sessionKeys(t *testing.T, bucket storage.ObjectStore, reg archive.SessionRegistration) []string {
	t.Helper()
	objects, err := bucket.List(context.Background(), "sessions/"+reg.Harness.Name+"/"+reg.ArchiveSessionID+"/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	return keys
}

func planJSONFor(t *testing.T, f *backfillFixture, args ...string) map[string]any {
	t.Helper()
	out, errOut, code := f.command(t, append([]string{"backfill", "--dry-run", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errOut)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func historyLine(t *testing.T, f *backfillFixture, id string) string {
	t.Helper()
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 0 {
		t.Fatalf("history: %d %s", code, errOut)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, id+" ") {
			return line
		}
	}
	t.Fatalf("history has no %s:\n%s", id, out)
	return ""
}

// The default undo: the plan, the prompt, and then every session of the
// latest import deleted from the bucket, metadata before sources and
// subagents before parents, forgotten with an undo removal record; only the
// projects and apps the import added are taken back. A later backfill skips
// the sessions unless --include-removed.
func TestBackfillUndoGolden(t *testing.T) {
	f, bucket := newUndoFixture(t)
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 11 || len(children) != 2 {
		t.Fatalf("%d sessions, %d subagents imported", len(parents), len(children))
	}
	for _, reg := range append(parents, children...) {
		if len(sessionKeys(t, bucket, reg)) < 2 {
			t.Fatalf("%s is not in the bucket", reg.ArchiveSessionID)
		}
	}
	cfgBefore, _, _ := config.Load(f.data)

	out, errOut, code := f.undoRun(t, strings.NewReader("y\n"), true)
	if code != 0 {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	f.checkPrivate(t, "output", out)
	checkGolden(t, "undo.txt", []byte(strings.ReplaceAll(out, f.root, "$ROOT")))

	// Deletion order.
	position := map[string]int{}
	for i, key := range bucket.deletes {
		position[key] = i
	}
	parentMetadata := map[string]int{}
	for _, reg := range append(parents, children...) {
		metadata, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		at, ok := position[metadata]
		if !ok {
			t.Fatalf("metadata of %s was not deleted", reg.ArchiveSessionID)
		}
		parentMetadata[reg.ArchiveSessionID] = at
		sources := 0
		for key, i := range position {
			if strings.HasPrefix(key, "sessions/"+reg.Harness.Name+"/"+reg.ArchiveSessionID+"/source.") {
				sources++
				if i < at {
					t.Errorf("%s deleted before its metadata", key)
				}
			}
		}
		if sources == 0 {
			t.Errorf("no source of %s was deleted", reg.ArchiveSessionID)
		}
		if keys := sessionKeys(t, bucket, reg); len(keys) != 0 {
			t.Errorf("%s left %v", reg.ArchiveSessionID, keys)
		}
	}
	for _, child := range children {
		if parentMetadata[child.ArchiveSessionID] > parentMetadata[child.ParentSessionID] {
			t.Errorf("subagent %s deleted after its parent", child.ArchiveSessionID)
		}
	}

	// Forgotten locally, with undo records; the hook-captured session stays.
	store := collector.OpenLocalStoreReadOnly(f.data)
	if p, c := importRegistrations(t, f.data, firstImport); len(p)+len(c) != 0 {
		t.Fatalf("%d sessions still registered", len(p)+len(c))
	}
	for _, reg := range parents {
		record, found, err := store.Removal(reg.Harness.Name, reg.NativeSessionID)
		if err != nil || !found || record.Reason != collector.RemovalReasonUndo || !record.At.Equal(backfillNow.UTC()) {
			t.Errorf("removal record of %s: %+v %v %v", reg.ArchiveSessionID, record, found, err)
		}
		if _, found, _ := store.ArchiveSessionID(reg.NativeSessionID); found {
			t.Errorf("%s is still indexed", reg.ArchiveSessionID)
		}
	}
	if _, found, _ := store.ArchiveSessionID("c-archived"); !found {
		t.Fatal("the hook-captured session was forgotten")
	}
	if candidates, _ := store.LoadSubagentCandidates(); len(candidates) != 0 {
		t.Fatalf("candidates left: %+v", candidates)
	}

	// Projects the import added are excluded and kept; the one that was
	// already included stays included. The apps it added are removed.
	cfg, _, _ := config.Load(f.data)
	if len(cfg.Archive.Projects) != 5 || len(cfg.ImportedHarnesses) != 0 {
		t.Fatalf("config: projects %+v, imported apps %v", cfg.Archive.Projects, cfg.ImportedHarnesses)
	}
	for i, p := range cfg.Archive.Projects {
		before := cfgBefore.Archive.Projects[i]
		if p.ProjectID != before.ProjectID || !p.ActivatedAt.Equal(before.ActivatedAt) {
			t.Errorf("project %d changed identity: %+v", i, p)
		}
		if wantIncluded := i == 0; p.Included != wantIncluded {
			t.Errorf("project %s included %v", p.Root, p.Included)
		}
	}
	b, _ := loadBatch(t, f.data, firstImport)
	if b.UndoneAt == nil || len(b.ProjectsExcluded) != 4 {
		t.Fatalf("batch %+v", b)
	}
	if line := historyLine(t, f, firstImport); !strings.HasSuffix(line, "  undone") {
		t.Fatalf("history: %q", line)
	}

	// A second undo finds nothing left, and changes nothing.
	before := snapshotAll(t, f, bucket.MemoryStore)
	out, errOut, code = f.undoRun(t, nil, false, "--yes", firstImport)
	if code != 0 || !strings.Contains(out, "Import "+firstImport+" has nothing left to undo.") {
		t.Fatalf("second undo: code %d, %s\n%s", code, errOut, out)
	}
	before.check(t, f, bucket.MemoryStore)

	// Backfill now skips them as removed by undo, unless --include-removed.
	skipped := planJSONFor(t, f)["skipped"].(map[string]any)
	if skipped["removed_by_undo"] != float64(11) {
		t.Fatalf("skipped %v", skipped)
	}
	again := planJSONFor(t, f, "--include-removed")
	if _, ok := again["skipped"].(map[string]any)["removed_by_undo"]; ok {
		t.Fatalf("--include-removed still skips: %v", again["skipped"])
	}
	projects := again["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("importable again: %v", projects)
	}
	sessions := projects[0].(map[string]any)["sessions"].(map[string]any)
	if sessions["claude"] != float64(2) || sessions["codex"] != float64(1) || sessions["cursor"] != float64(1) {
		t.Fatalf("importable again: %v", projects[0])
	}
}

// --project limits undo to one project: only its sessions go, and only its
// exclusion if the import added it. A project that was already included
// stays included. A later undo of the rest finishes the import.
func TestBackfillUndoProject(t *testing.T) {
	f, bucket := newUndoFixture(t)
	levenshtein := filepath.Join(f.userHome, "levenshtein")
	all, allChildren := importRegistrations(t, f.data, firstImport)

	out, errOut, code := f.undoRun(t, nil, false, "--project", levenshtein, "--yes")
	if code != 0 {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "Undo import "+firstImport+" in ~/levenshtein") || !strings.Contains(out, "1 project the import added is excluded") {
		t.Fatalf("output:\n%s", out)
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 8 || len(children) != 2 {
		t.Fatalf("left: %d sessions, %d subagents", len(parents), len(children))
	}
	for _, reg := range parents {
		if reg.ProjectRoot == levenshtein {
			t.Errorf("%s in levenshtein is still registered", reg.ArchiveSessionID)
		}
	}
	for _, reg := range all {
		gone := len(sessionKeys(t, bucket, reg)) == 0
		if gone != (reg.ProjectRoot == levenshtein) {
			t.Errorf("%s in %s: gone from the bucket %v", reg.ArchiveSessionID, reg.ProjectRoot, gone)
		}
	}
	cfg, _, _ := config.Load(f.data)
	for _, p := range cfg.Archive.Projects {
		if p.Included == (p.Root == levenshtein) {
			t.Errorf("project %s included %v", p.Root, p.Included)
		}
	}
	if strings.Join(cfg.ImportedHarnesses, ",") != "codex,cursor" {
		t.Fatalf("imported apps %v", cfg.ImportedHarnesses)
	}
	if line := historyLine(t, f, firstImport); !strings.Contains(line, "partly undone; 8 sessions left") {
		t.Fatalf("history: %q", line)
	}

	// The project that was included before the import keeps its inclusion.
	agentArchive := filepath.Join(f.userHome, "agent-archive")
	out, errOut, code = f.undoRun(t, nil, false, firstImport, "--project", agentArchive, "--yes")
	if code != 0 || strings.Contains(out, "excluded from capture") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "4 sessions and 2 subagent transcripts are deleted") {
		t.Fatalf("output:\n%s", out)
	}
	cfg, _, _ = config.Load(f.data)
	if !cfg.Archive.Projects[0].Included || cfg.Archive.Projects[0].Root != agentArchive {
		t.Fatalf("agent-archive: %+v", cfg.Archive.Projects[0])
	}
	// The only Codex import was in agent-archive; a Cursor one is left in
	// ~/personal-website.
	if strings.Join(cfg.ImportedHarnesses, ",") != "cursor" {
		t.Fatalf("imported apps %v", cfg.ImportedHarnesses)
	}
	for _, child := range allChildren {
		if keys := sessionKeys(t, bucket, child); len(keys) != 0 {
			t.Errorf("subagent left %v", keys)
		}
	}

	// The rest.
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("rest: code %d, %s", code, errOut)
	}
	if p, c := importRegistrations(t, f.data, firstImport); len(p)+len(c) != 0 {
		t.Fatalf("%d left", len(p)+len(c))
	}
	cfg, _, _ = config.Load(f.data)
	for _, p := range cfg.Archive.Projects {
		if p.Included != (p.Root == agentArchive) {
			t.Errorf("project %s included %v", p.Root, p.Included)
		}
	}
	if line := historyLine(t, f, firstImport); !strings.HasSuffix(line, "  undone") {
		t.Fatalf("history: %q", line)
	}
}

// A failed delete leaves that session registered and in the bucket, and undo
// goes on with the others; running undo again finishes the job.
func TestBackfillUndoPartialFailure(t *testing.T) {
	f, bucket := newUndoFixture(t)
	parents, _ := importRegistrations(t, f.data, firstImport)
	failing := parents[0]
	bucket.reset(failing.ArchiveSessionID)

	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 1 || !strings.Contains(errOut, "1 session could not be removed") || !strings.Contains(errOut, "backfill undo "+firstImport+" again") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	f.checkPrivate(t, "errors", errOut)
	left, children := importRegistrations(t, f.data, firstImport)
	if len(left) != 1 || left[0].ArchiveSessionID != failing.ArchiveSessionID || len(children) != 0 {
		t.Fatalf("left %d sessions and %d subagents", len(left), len(children))
	}
	if len(sessionKeys(t, bucket, failing)) == 0 {
		t.Fatal("the failed session is gone from the bucket")
	}
	if _, found, _ := collector.OpenLocalStoreReadOnly(f.data).Removal(failing.Harness.Name, failing.NativeSessionID); found {
		t.Fatal("the failed session has a removal record")
	}
	if line := historyLine(t, f, firstImport); !strings.Contains(line, "partly undone; 1 session left") {
		t.Fatalf("history: %q", line)
	}

	bucket.reset("")
	out, errOut, code = f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Undo import "+firstImport) || !strings.Contains(out, "Deleted 1 session from the archive") {
		t.Fatalf("rerun: code %d, %s\n%s", code, errOut, out)
	}
	if len(sessionKeys(t, bucket, failing)) != 0 {
		t.Fatal("the rerun left the session in the bucket")
	}
	if line := historyLine(t, f, firstImport); !strings.HasSuffix(line, "  undone") {
		t.Fatalf("history: %q", line)
	}
}

// A session resumed after the import is deleted with its newer content,
// every source it published included, and the plan counts it as resumed.
func TestBackfillUndoResumedSession(t *testing.T) {
	f, bucket := newUndoFixture(t)
	store := collector.OpenLocalStoreReadOnly(f.data)
	id, found, _ := store.ArchiveSessionID("c-lev-1")
	if !found {
		t.Fatal("c-lev-1 not imported")
	}
	reg, _, _ := store.LoadRegistration(id)
	file, err := os.OpenFile(reg.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"user","uuid":"c","sessionId":"c-lev-1","timestamp":"2026-09-23T20:00:00Z","message":{"role":"user","content":"one more thing"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	resumedAt := backfillNow.Add(time.Hour)
	if err := handleHookEvent(f.data, "claude", map[string]any{
		"hook_event_name": "Stop", "session_id": "c-lev-1", "cwd": reg.ProjectRoot, "transcript_path": reg.TranscriptPath,
		"last_assistant_message": "Done again.",
	}, resumedAt); err != nil {
		t.Fatal(err)
	}
	f.env.Now = func() time.Time { return resumedAt }
	if _, errOut, code := f.command(t, "sync"); code != 0 {
		t.Fatalf("sync: %s", errOut)
	}
	keys := sessionKeys(t, bucket, reg)
	sources := 0
	for _, key := range keys {
		if strings.Contains(key, "/source.") {
			sources++
		}
	}
	if sources < 2 {
		t.Fatalf("the resumed session did not publish newer content: %v", keys)
	}

	out, errOut, code := f.undoRun(t, strings.NewReader("y\n"), true)
	if code != 0 {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "This includes 1 session resumed since the import, with its newer content.") {
		t.Fatalf("output:\n%s", out)
	}
	if keys := sessionKeys(t, bucket, reg); len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}
	if _, found, _ := store.LoadRegistration(id); found {
		t.Fatal("still registered")
	}
}

// Undo holds collector.lock while it removes sessions, so no collector pass
// can republish one mid-undo.
func TestBackfillUndoHoldsCollectorLock(t *testing.T) {
	f, _ := newUndoFixture(t)
	checked := false
	backfillCheckpoint = func(step string) error {
		if step != "undoing" {
			return nil
		}
		checked = true
		if _, err := runPass(f.env, false, passOptions{}); !errors.Is(err, local.ErrBusy) {
			t.Errorf("a collector pass ran during undo: %v", err)
		}
		if release, err := local.NamedLock(f.data, "setup.lock"); err == nil {
			release()
			t.Error("setup.lock was free during undo")
		}
		return nil
	}
	t.Cleanup(func() { backfillCheckpoint = nil })
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("code %d, %s", code, errOut)
	}
	if !checked {
		t.Fatal("checkpoint not reached")
	}
}

// Sessions whose objects are in a destination this machine no longer uses
// are only forgotten locally: the current bucket is not called for them.
func TestBackfillUndoPreviousDestination(t *testing.T) {
	f, bucket := newUndoFixture(t)
	cfg, _, _ := config.Load(f.data)
	cfg.PreviousDestinations = append(cfg.PreviousDestinations, cfg.Storage)
	cfg.Storage = credentials.Config{Provider: credentials.ProviderS3, Bucket: "new-bucket", Region: "us-east-1", AWSProfile: "test"}
	cfg.DestinationSince = backfillNow.Add(time.Hour).UTC()
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	parents, _ := importRegistrations(t, f.data, firstImport)
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("undo opened the bucket")
		return bucket, nil
	}
	before := bucketSnapshot(t, bucket.MemoryStore)

	out, errOut, code := f.undoRun(t, strings.NewReader("y\n"), true)
	if code != 0 {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"11 sessions and 2 subagent transcripts from a previous storage destination are forgotten", "Forget 11 sessions? This cannot be undone. [y/N]", "Forgot 11 sessions and 2 subagent transcripts from a previous storage destination"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if bucketSnapshot(t, bucket.MemoryStore) != before || len(bucket.deletes) != 0 {
		t.Fatal("the bucket changed")
	}
	store := collector.OpenLocalStoreReadOnly(f.data)
	for _, reg := range parents {
		if record, found, _ := store.Removal(reg.Harness.Name, reg.NativeSessionID); !found || record.Reason != collector.RemovalReasonUndo {
			t.Errorf("%s: no undo record", reg.ArchiveSessionID)
		}
	}
	if p, c := importRegistrations(t, f.data, firstImport); len(p)+len(c) != 0 {
		t.Fatalf("%d still registered", len(p)+len(c))
	}
}

// Declining, running out of input, and having no terminal without --yes all
// change nothing, locally or in the bucket; so do refusals.
func TestBackfillUndoChangesNothingUnlessConfirmed(t *testing.T) {
	f, bucket := newUndoFixture(t)
	before := snapshotAll(t, f, bucket.MemoryStore)
	config0, _ := os.ReadFile(filepath.Join(f.data, "config.json"))
	check := func(name string) {
		t.Helper()
		before.check(t, f, bucket.MemoryStore)
		if after, _ := os.ReadFile(filepath.Join(f.data, "config.json")); !bytes.Equal(after, config0) {
			t.Fatalf("%s: config.json changed", name)
		}
		if len(bucket.deletes) != 0 {
			t.Fatalf("%s: deleted %v", name, bucket.deletes)
		}
	}
	for _, answer := range []string{"\n", "n\n", "maybe\nno\n"} {
		out, errOut, code := f.undoRun(t, strings.NewReader(answer), true)
		if code != 0 || !strings.Contains(out, "Delete 11 sessions from the archive? This cannot be undone. [y/N]") || !strings.Contains(out, "Cancelled. Nothing was changed.") {
			t.Fatalf("%q: code %d, %s\n%s", answer, code, errOut, out)
		}
		check(answer)
	}
	if _, errOut, code := f.undoRun(t, strings.NewReader(""), true); code != 1 || !strings.Contains(errOut, "Nothing was changed.") {
		t.Fatalf("no input: code %d, %s", code, errOut)
	}
	check("no input")
	if out, errOut, code := f.undoRun(t, strings.NewReader("y\n"), false); code != 1 || out != "" || !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "--yes") {
		t.Fatalf("no terminal: code %d, %q, %q", code, out, errOut)
	}
	check("no terminal")
	if _, errOut, code := f.undoRun(t, nil, false, "--yes", "2020-01-01-1"); code != 1 || !strings.Contains(errOut, `no import "2020-01-01-1"`) || !strings.Contains(errOut, "backfill history") {
		t.Fatalf("unknown ID: code %d, %s", code, errOut)
	}
	check("unknown ID")

	if _, err := config.SetPaused(f.data, true); err != nil {
		t.Fatal(err)
	}
	before, config0 = snapshotAll(t, f, bucket.MemoryStore), mustRead(t, filepath.Join(f.data, "config.json"))
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 1 || !strings.Contains(errOut, "paused") {
		t.Fatalf("paused: code %d, %s", code, errOut)
	}
	check("paused")
	if _, err := config.SetPaused(f.data, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath(f.data), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, config0 = snapshotAll(t, f, bucket.MemoryStore), mustRead(t, filepath.Join(f.data, "config.json"))
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 1 || !strings.Contains(errOut, "setup needs recovery") {
		t.Fatalf("setup pending: code %d, %s", code, errOut)
	}
	check("setup pending")

	// No imports at all.
	g := newBackfillFixture(t)
	if out, errOut, code := g.importRun(t, nil, false, "undo", "--yes"); code != 0 || !strings.Contains(out, "No imports to undo.") {
		t.Fatalf("no imports: code %d, %s\n%s", code, errOut, out)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A session the import gained while the prompt was open (the collector
// registered a subagent, say) stops the undo before anything is removed.
func TestBackfillUndoSelectionGrew(t *testing.T) {
	f, bucket := newUndoFixture(t)
	parents, _ := importRegistrations(t, f.data, firstImport)
	parent := parents[0]
	stdin := &onFirstRead{r: strings.NewReader("y\n"), before: func() {
		store, err := collector.NewLocalStore(f.data)
		if err != nil {
			t.Fatal(err)
		}
		late := parent
		late.ArchiveSessionID, late.NativeSessionID = "late-subagent", parent.NativeSessionID+":subagent:late"
		late.ParentSessionID, late.ParentNativeSessionID = parent.ArchiveSessionID, parent.NativeSessionID
		if err := store.SaveRegistration(late); err != nil {
			t.Fatal(err)
		}
	}}
	before := bucketSnapshot(t, bucket.MemoryStore)
	config0 := mustRead(t, filepath.Join(f.data, "config.json"))
	out, errOut, code := f.undoRun(t, stdin, true)
	if code != 1 || !strings.Contains(errOut, "the import changed while this was open; run undo again") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if bucketSnapshot(t, bucket.MemoryStore) != before || !bytes.Equal(mustRead(t, filepath.Join(f.data, "config.json")), config0) {
		t.Fatal("changed")
	}
	if p, _ := importRegistrations(t, f.data, firstImport); len(p) != 11 {
		t.Fatalf("%d left", len(p))
	}
	if b, _ := loadBatch(t, f.data, firstImport); b.UndoneAt != nil {
		t.Fatal("batch marked undone")
	}
}

// Undo removes the latest import by default; an older one is named by its
// ID, which may come before or after the flags. An app another import still
// needs stays in ImportedHarnesses.
func TestBackfillUndoLatestByDefault(t *testing.T) {
	const secondImport = "2026-09-23-2"
	f, bucket := newUndoFixture(t, "--harness", "codex")
	if _, errOut, code := f.importRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("second import: %s", errOut)
	}
	first, _ := importRegistrations(t, f.data, firstImport)
	second, _ := importRegistrations(t, f.data, secondImport)
	if len(first) != 1 || len(second) != 10 {
		t.Fatalf("imported %d then %d", len(first), len(second))
	}
	cfg, _, _ := config.Load(f.data)
	if strings.Join(cfg.ImportedHarnesses, ",") != "codex,cursor" {
		t.Fatalf("imported apps %v", cfg.ImportedHarnesses)
	}

	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Undo import "+secondImport) {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if p, _ := importRegistrations(t, f.data, secondImport); len(p) != 0 {
		t.Fatalf("%d of the latest import left", len(p))
	}
	if p, _ := importRegistrations(t, f.data, firstImport); len(p) != 1 || len(sessionKeys(t, bucket, p[0])) == 0 {
		t.Fatal("the older import was touched")
	}
	// Cursor came only with the latest import; Codex is still needed.
	cfg, _, _ = config.Load(f.data)
	if strings.Join(cfg.ImportedHarnesses, ",") != "codex" {
		t.Fatalf("imported apps %v", cfg.ImportedHarnesses)
	}
	if !strings.Contains(out, "Cursor imports without hooks are no longer published.") {
		t.Fatalf("output:\n%s", out)
	}

	// The default is still the latest import, which has nothing left.
	if out, _, code := f.undoRun(t, nil, false, "--yes"); code != 0 || !strings.Contains(out, "Import "+secondImport+" has nothing left to undo.") {
		t.Fatalf("latest again: %d\n%s", code, out)
	}
	if out, errOut, code := f.undoRun(t, nil, false, "--yes", firstImport); code != 0 || !strings.Contains(out, "Undo import "+firstImport) {
		t.Fatalf("by ID: code %d, %s\n%s", code, errOut, out)
	}
	cfg, _, _ = config.Load(f.data)
	if len(cfg.ImportedHarnesses) != 0 {
		t.Fatalf("imported apps %v", cfg.ImportedHarnesses)
	}
	for _, id := range []string{firstImport, secondImport} {
		if line := historyLine(t, f, id); !strings.HasSuffix(line, "  undone") {
			t.Fatalf("history: %q", line)
		}
	}
	batches, _ := backfill.LoadBatches(f.data)
	if len(batches) != 2 {
		t.Fatalf("%d batches", len(batches))
	}
}

// Undo selects sessions by their registrations: an ID the batch file lists
// that was never registered is ignored, and the import is still undone.
func TestBackfillUndoIgnoresUnregisteredBatchEntries(t *testing.T) {
	f, _ := newUndoFixture(t)
	b, _ := loadBatch(t, f.data, firstImport)
	b.Sessions = append(b.Sessions, "never-registered")
	b.Subagents = append(b.Subagents, "never-materialized")
	if err := backfill.SaveBatch(f.data, b); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "11 sessions and 2 subagent transcripts are deleted") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if line := historyLine(t, f, firstImport); !strings.HasSuffix(line, "  undone") {
		t.Fatalf("history: %q", line)
	}
}

// An undone import is never continued: a later backfill with the same
// options starts a new one.
func TestBackfillUndoneImportNotContinued(t *testing.T) {
	f, _ := newImportFixture(t)
	backfillCheckpoint = func(step string) error {
		if step == "registered" {
			return errors.New("simulated crash")
		}
		return nil
	}
	backfillHoldSteps = 3
	t.Cleanup(func() { backfillCheckpoint, backfillHoldSteps = nil, 0 })
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 {
		t.Fatal("no crash")
	}
	backfillCheckpoint = nil
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("undo: %s", errOut)
	}
	b, _ := loadBatch(t, f.data, firstImport)
	if b.CompletedAt != nil || b.UndoneAt == nil {
		t.Fatalf("batch %+v", b)
	}
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background")
	if code != 0 || !strings.Contains(out, "as import 2026-09-23-2") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	if !slices.ContainsFunc(strings.Split(out, "\n"), func(line string) bool { return strings.Contains(line, "Registered") }) {
		t.Fatalf("output:\n%s", out)
	}
}
