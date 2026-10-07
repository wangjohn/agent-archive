//go:build darwin || linux

package backfill

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestDurableLiveCursorAdmissionPublishesAfterSourceDeletion(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	home, project, native := t.TempDir(), t.TempDir(), t.TempDir()
	at := fixedNow.UTC()
	start := at.Add(-time.Hour)
	cfg := config.Config{DurableImportProtection: true, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: start}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(native, "space ü", "state.vscdb")
	writeCursorDB(t, path, true, chatRows("c", map[string]any{"createdAt": millis(start), "workspaceIdentifier": map[string]any{"id": "synthetic", "uri": "file://" + project}}, "synthetic question", "synthetic response"))
	w := startCursorWriter(t, path, true)
	w.do("extra")
	catalog := readCursorDatabase(t.Context(), testSources, path, cursorstore.Options{})
	if !catalog.Checked {
		t.Fatal(catalog.Reason)
	}
	var candidate Candidate
	for _, chat := range catalog.Chats {
		if chat.ID == "c" {
			candidate = Candidate{Harness: "cursor", NativeSessionID: "c", SourceKind: archive.SourceKindCursorSQLite, SourceKey: "c", ProjectRoot: project, StartedAt: chat.CreatedAt, StartedAtSource: archive.StartedAtSourceCursorComposer, reviewedChat: &chat}
		}
	}
	if candidate.reviewedChat == nil {
		t.Fatal("reviewed native facts absent")
	}
	result, err := (Registration{Durable: true, Sources: testSources, Home: home, Store: store, Batch: "synthetic", AdmittedAt: at, CursorDatabase: path}).Run([]Candidate{candidate})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatal(result, err)
	}
	w.kill()
	if err = os.RemoveAll(native); err != nil {
		t.Fatal(err)
	}
	store = state.OpenReadOnly(home)
	cloud := storagetest.NewMemoryStore()
	resultID := result.Sessions[0]
	published, err := collector.Run(t.Context(), store, cloud, collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", RepoKey: func(string) string { t.Fatal("staged cursor publication reran Git"); return "" }})
	if err != nil || len(published.Errors) != 0 || len(published.Published) != 1 {
		t.Fatal(published, err)
	}
	reg, found, err := store.LoadRegistration(resultID)
	if err != nil || !found {
		t.Fatal(reg, found, err)
	}
	released, err := store.AdmissionStageReleased(reg)
	if err != nil || !released {
		t.Fatal("restart did not release published live evidence", released, err)
	}
}
