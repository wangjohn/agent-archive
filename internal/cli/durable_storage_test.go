package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"testing"
)

func TestFuturePublishedStatusGrantsNoPublicationAuthority(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "published", "foreign.json"), []byte(`{"commit":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	skips := 0
	sessions := statusSessions{store: store, skip: func(id string, err error) {
		if id != "foreign" || !errors.Is(err, state.ErrDurableStorageRecovery) {
			t.Fatalf("skip: %s %v", id, err)
		}
		skips++
	}}
	reg := archive.SessionRegistration{ArchiveSessionID: "foreign", Harness: archive.Harness{Name: "claude"}}
	var app appStatus
	var pair projectCaptureStatus
	var readback verificationOutcome
	sessions.addSession(&app, &pair, reg, config.Config{}, home, nil, &readback)
	sessions.addPublication(&app, &pair, reg, config.Config{}, home, &readback)
	if skips != 2 || app.CapturedLocally || app.Published || app.PublishedSessions != 0 || pair.CapturedLocally || pair.Published {
		t.Fatalf("refusal gained authority: skips=%d app=%+v pair=%+v", skips, app, pair)
	}
}

func TestStatusSourceOnlyRecoveryWithoutRegistration(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	data := []byte("original")
	sum := sha256.Sum256(data)
	if _, err = store.StagePendingSource("orphan", archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data), Key: "synthetic"}, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var view statusView
	sessions := readSessionStatus(&view, config.Config{}, home, store)
	if len(sessions.regs) != 0 || view.Collector.PendingCount != 1 || len(view.Warnings) == 0 {
		t.Fatalf("source-only status: %+v regs=%v", view.Collector, sessions.regs)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("status changed config", err)
	}
}

func TestStatusAnonymousGenerationAndPendingTempsAreOwed(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err = config.WithDurableStorage(home, func(g config.DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"pending", "generation-recovery"} {
		if err = os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(home, dir, ".pending-123"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var view statusView
	sessions := readSessionStatus(&view, config.Config{}, home, store)
	if len(sessions.regs) != 0 || view.Collector.PendingCount != 2 || len(view.Warnings) < 2 {
		t.Fatalf("anonymous work omitted: %+v %v", view.Collector, view.Warnings)
	}
}
