package backfill

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNativeChildImportedNowBelongsToNewBatchAndUndoRetainsOldParent(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	at := fixedNow.UTC()
	cfg := config.Config{ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: at}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentID := "00000000-0000-0000-0000-000000000001"
	childID := "00000000-0000-0000-0000-000000000002"
	parentPath := filepath.Join(root, "parent.jsonl")
	childPath := filepath.Join(root, "child.jsonl")
	for _, path := range []string{parentPath, childPath} {
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := Registration{Home: home, Store: store, Batch: "2026-09-22-1", AdmittedAt: at}
	oldResult, err := old.Run([]Candidate{{Harness: "codex", NativeSessionID: parentID, TranscriptPath: parentPath, ProjectRoot: root, StartedAt: at.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}})
	if err != nil || len(oldResult.Sessions) != 1 {
		t.Fatalf("parent import %+v %v", oldResult, err)
	}
	newer := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: at.Add(time.Minute)}
	result, err := newer.Run([]Candidate{{Harness: "codex", NativeSessionID: childID, NativeChild: true, ParentNativeID: parentID, RootNativeID: parentID, NativeHome: root, TranscriptPath: childPath, ProjectRoot: root, StartedAt: at.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}})
	if err != nil || len(result.Sessions) != 0 || len(result.Subagents) != 1 {
		t.Fatalf("native import result %+v %v", result, err)
	}
	child, found, err := store.LoadRegistration(result.Subagents[0])
	if err != nil || !found || child.NativeSessionID != childID || !child.InBatch(newer.Batch) || child.InBatch(old.Batch) || !child.HookObservedAt.IsZero() {
		t.Fatalf("child membership %+v %v", child, err)
	}
	batch := Batch{ID: newer.Batch}
	if err := batch.Reconcile(store); err != nil || len(batch.Subagents) != 1 || len(batch.Sessions) != 0 {
		t.Fatalf("reconciliation %+v %v", batch, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	selected := selectUndoRegistrations(regs, newer.Batch, map[string]bool{root: true})
	if len(selected) != 1 || selected[0].ArchiveSessionID != child.ArchiveSessionID {
		t.Fatalf("undo leaked into old batch: %+v", selected)
	}
}

func TestNestedUndoOrdersDescendantsAndRejectsCycles(t *testing.T) {
	sessions := []UndoSession{{Registration: archive.SessionRegistration{ArchiveSessionID: "root"}}, {Registration: archive.SessionRegistration{ArchiveSessionID: "child", ParentSessionID: "root"}}, {Registration: archive.SessionRegistration{ArchiveSessionID: "grandchild", ParentSessionID: "child"}}}
	ordered, err := orderUndoChildrenFirst(sessions)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"grandchild", "child", "root"} {
		if ordered[i].Registration.ArchiveSessionID != id {
			t.Fatalf("delete order %+v", ordered)
		}
	}
	ordered[2].Registration.ParentSessionID = "grandchild"
	if _, err := orderUndoChildrenFirst(ordered); err == nil {
		t.Fatal("cyclic ownership accepted for deletion")
	}
}

func TestNestedUndoRejectsOverdepthRegardlessOfMemoizedParentOrder(t *testing.T) {
	var sessions []UndoSession
	for i := range 66 {
		parent := ""
		if i > 0 {
			parent = strconv.Itoa(i - 1)
		}
		reg := archive.SessionRegistration{ArchiveSessionID: strconv.Itoa(i), ParentSessionID: parent}
		sessions = append(sessions, UndoSession{Registration: reg})
	}
	for range 50 {
		if _, err := orderUndoChildrenFirst(sessions); err == nil {
			t.Fatal("overdepth relationship accepted")
		}
	}
}
