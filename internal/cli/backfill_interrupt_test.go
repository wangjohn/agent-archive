package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// Ctrl-C during planning cancels the plan's context (BuildPlan then removes
// its copy of Cursor's database on the way out), and stops the watch so a
// second Ctrl-C ends the process as usual.
func TestPlanningInterruptCancelsContextAndStopsWatching(t *testing.T) {
	signals := make(chan os.Signal, 1)
	stopped := make(chan struct{})
	env := Env{Interrupts: func() (<-chan os.Signal, func()) {
		return signals, func() { close(stopped) }
	}}
	ctx, stop := interruptibleContext(env)
	signals <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not cancel planning")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch kept Ctrl-C after the first one")
	}
	stop() // stops the watch once only; a second stop would panic on close
}

// Ctrl-C during `backfill --dry-run` stops planning: nothing is printed as a
// plan, nothing is written, and no copy of Cursor's database is left in the
// snapshot folder.
func TestBackfillPlanningStopsOnInterrupt(t *testing.T) {
	f := newBackfillFixture(t)
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt // pressed as planning starts
	f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	before := snapshotTree(t, f.data)
	out, errOut, code := f.command(t, "backfill", "--dry-run")
	if code != 1 || !strings.Contains(errOut, "stopped. Nothing was changed.") || strings.Contains(out, "Dry run") {
		t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if after := snapshotTree(t, f.data); after != before {
		t.Fatalf("data directory changed:\n%s\n---\n%s", before, after)
	}
	root := filepath.Join(cursorstore.SnapshotTempDirForTesting, fmt.Sprintf("agent-archive-cursor-%d", os.Getuid()))
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		t.Fatalf("a Cursor database copy was left: %v", entries)
	}
}

// Without Ctrl-C, stop ends the watch and cancels the context.
func TestPlanningInterruptStopWithoutSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	stops := 0
	env := Env{Interrupts: func() (<-chan os.Signal, func()) { return signals, func() { stops++ } }}
	ctx, stop := interruptibleContext(env)
	if ctx.Err() != nil {
		t.Fatal("cancelled before any signal")
	}
	stop()
	if ctx.Err() == nil || stops != 1 {
		t.Fatalf("err %v, stops %d", ctx.Err(), stops)
	}
}
