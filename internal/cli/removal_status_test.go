package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStatusAndSyncTreatRemovalAsRecoveryNotUpload(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env, home, _, bucket := publishedThroughSync(t, at)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatal(regs, err)
	}
	reg := regs[0]
	if err := retention.DeleteOwnedSession(t.Context(), store, bucket, reg, state.RemovalReasonUndo, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var view statusView
	sessions := readSessionStatus(&view, cfg, home, store)
	app := sessions.appStatus(reg.Harness.Name, cfg, home, nil)
	if app.PendingRemovalSessions != 1 || app.UploadingSessions != 0 || len(app.Uploading) != 0 || view.Collector.PendingCount != 1 {
		t.Fatal("removal described as upload or omitted", app, view.Collector)
	}
	raw, err := json.Marshal(app)
	if err != nil || !strings.Contains(string(raw), `"pending_removal_sessions":1`) || !strings.Contains(appCounts(app), "1 removal pending") {
		t.Fatal("removal lacks truthful JSON/text", string(raw), err)
	}
	blocking, _, err := pendingSessionCounts(home, cfg)
	if err != nil || blocking != 1 {
		t.Fatal("setup destination guard omitted removal", blocking, err)
	}
	result, err := finishPassWithRetention(home, env, cfg, store, bucket, time.Time{}, collector.Result{}, "", nil)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("resumable deletion reported upload failure", result, err)
	}
	current, err := store.LoadStatus()
	if err != nil || current.PendingCount != 0 || len(current.SessionIssues) != 0 {
		t.Fatal("terminal removal remained pending", current, err)
	}
	if ids, err := store.OrphanedSessions(nil); err != nil || len(ids) != 0 {
		t.Fatal("terminal control remained recovery", ids, err)
	}
}

func TestStatusKeepsUploadObligationAlongsideRemoval(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, home, _, bucket := publishedThroughSync(t, at)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatal(regs, err)
	}
	reg := regs[0]
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, _ := published.LastPublished()
	ref, found := published.LastPublishedSource()
	if !found {
		t.Fatal("missing selecting source")
	}
	key := "sessions/" + reg.Harness.Name + "/" + reg.ArchiveSessionID + "/metadata.json"
	pending := state.PendingPublication{Bundle: bundle, SourceKey: ref.Key, SourceSHA256: ref.SHA256, SourceSize: ref.CompressedBytes, MetadataOnly: true, MetadataKey: key, MetadataBytes: published.Metadata()}
	if err = store.SavePending(reg.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	if err = retention.DeleteOwnedSession(t.Context(), store, bucket, reg, state.RemovalReasonUndo, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var view statusView
	sessions := readSessionStatus(&view, cfg, home, store)
	app := sessions.appStatus(reg.Harness.Name, cfg, home, nil)
	if app.PendingRemovalSessions != 1 || app.UploadingSessions != 1 {
		t.Fatal("removal hid existing publication work", app)
	}
}
