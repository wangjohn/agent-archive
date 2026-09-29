package backfill

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSelectUndoRegistrationsIncludesChildrenAndOrphans(t *testing.T) {
	t.Parallel()
	const batch = "2026-09-23-1"
	reg := func(id, root, parent, importID string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, ProjectRoot: root, ParentSessionID: parent,
			Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(importID),
		}
	}
	regs := []archive.SessionRegistration{
		reg("parent-p", "/p", "", batch),
		reg("child-p", "/other", "parent-p", batch),
		reg("parent-q", "/q", "", batch),
		reg("child-q", "/q", "parent-q", batch),
		reg("orphan-p", "/p", "missing", batch),
		reg("orphan-q", "/q", "missing", batch),
		reg("other-batch", "/p", "", "2026-09-23-2"),
	}
	got := selectUndoRegistrations(regs, batch, map[string]bool{"/p": true})
	want := []string{"parent-p", "child-p", "orphan-p"}
	if len(got) != len(want) {
		t.Fatalf("selected %d registrations, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ArchiveSessionID != id {
			t.Fatalf("selected[%d] = %s, want %s", i, got[i].ArchiveSessionID, id)
		}
	}
}

func TestUndoAppsToRemoveKeepsAppsNeededByOtherImports(t *testing.T) {
	t.Parallel()
	const batch = "2026-09-23-1"
	reg := func(id, app, importID string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, Harness: archive.Harness{Name: app},
			Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(importID),
		}
	}
	regs := []archive.SessionRegistration{
		reg("claude-removed", "claude", batch),
		reg("codex-removed", "codex", batch),
		reg("codex-other", "codex", "2026-09-23-2"),
	}
	sessions := []UndoSession{{Registration: regs[0]}, {Registration: regs[1]}}
	cfg := config.Config{ImportedHarnesses: []string{"claude", "codex"}}
	b := Batch{AppsAdded: []string{"claude", "codex"}}
	got := undoAppsToRemove(cfg, b, regs, sessions)
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("apps to remove = %v, want [claude]", got)
	}
}
