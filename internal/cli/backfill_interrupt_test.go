package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// stubExit returns a stand-in for exitOnSignal, to set as Env.exitOnSignal,
// and a function returning the signals it was called with; like the real
// one it removes this process's snapshots first.
func stubExit() (func(os.Signal), func() []os.Signal) {
	exits := make(chan os.Signal, 8)
	exit := func(sig os.Signal) {
		cursorstore.RemoveOwnSnapshots()
		exits <- sig
	}
	return exit, func() []os.Signal {
		var got []os.Signal
		for {
			select {
			case sig := <-exits:
				got = append(got, sig)
			default:
				return got
			}
		}
	}
}

// Regression: 2026-09 review B-24. The first Ctrl-C during planning cancels
// it and says so at once (it used to print nothing, so people pressed it
// again). The watch keeps going, so the second Ctrl-C quits through
// exitOnSignal, which removes this process's copies of Cursor's database
// first, instead of the default handler leaving them behind.
func TestPlanningInterruptCancelsAndSecondOneExits(t *testing.T) {
	exit, exits := stubExit()
	signals := make(chan os.Signal, 1)
	stopped := make(chan struct{})
	env := Env{exitOnSignal: exit, Interrupts: func() (<-chan os.Signal, func()) {
		return signals, func() { close(stopped) }
	}}
	out := &syncBuffer{}
	ctx, stop := interruptibleContext(env, out)
	signals <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not cancel planning")
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "stopping; press Ctrl-C again to quit") })
	select {
	case <-stopped:
		t.Fatal("the watch let go of the signals after the first Ctrl-C")
	default:
	}
	signals <- os.Interrupt
	var got []os.Signal
	waitFor(t, func() bool { got = append(got, exits()...); return len(got) > 0 })
	if got[0] != os.Interrupt {
		t.Fatalf("exit on %v", got)
	}
	stop() // stops the watch once only; a second stop would panic on close
	<-stopped
}

// B-24: exitOnSignal removes this process's copies of Cursor's database
// before it exits, with the shell's status for the signal.
func TestExitOnSignalRemovesSnapshotsThenExits(t *testing.T) {
	for sig, want := range map[os.Signal]int{os.Interrupt: 130, syscall.SIGTERM: 143, syscall.SIGHUP: 129} {
		var steps []string
		exitAfterSignal(sig, func() { steps = append(steps, "remove") }, func(code int) { steps = append(steps, fmt.Sprintf("exit %d", code)) })
		if got := strings.Join(steps, ", "); got != fmt.Sprintf("remove, exit %d", want) {
			t.Fatalf("%v: %s", sig, got)
		}
	}
}

// B-24: SIGTERM or SIGHUP (a closing terminal, a process manager) quits at
// once, even as the first signal, through exitOnSignal.
func TestTerminateSignalsExitAtOnce(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		exit, exits := stubExit()
		signals := make(chan os.Signal, 1)
		env := Env{exitOnSignal: exit, Interrupts: func() (<-chan os.Signal, func()) { return signals, func() {} }}
		_, stop := interruptibleContext(env, &syncBuffer{})
		signals <- sig
		var got []os.Signal
		waitFor(t, func() bool { got = append(got, exits()...); return len(got) > 0 })
		if got[0] != sig {
			t.Fatalf("exit on %v, want %v", got, sig)
		}
		stop()
	}
}

// Ctrl-C during `backfill --dry-run` stops planning: nothing is printed as a
// plan, nothing is written, the stop is announced, and no copy of Cursor's
// database is left in the snapshot folder.
func TestBackfillPlanningStopsOnInterrupt(t *testing.T) {
	f := newBackfillFixture(t)
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt // pressed as planning starts
	f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	before := snapshotTree(t, f.data)
	out, errOut, code := f.command(t, "backfill", "--dry-run")
	if code != 1 || !strings.Contains(errOut, "stopped. Nothing was changed.") || !strings.Contains(errOut, "stopping; press Ctrl-C again to quit") || strings.Contains(out, "Dry run") {
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

// A Ctrl-C already waiting when planning starts has cancelled the context by
// the time interruptibleContext returns, before any planning.
func TestPlanningInterruptAlreadyPending(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	stops := 0
	env := Env{Interrupts: func() (<-chan os.Signal, func()) { return signals, func() { stops++ } }}
	var out bytes.Buffer
	ctx, stop := interruptibleContext(env, &out)
	if ctx.Err() == nil {
		t.Fatal("a pending Ctrl-C did not cancel before planning")
	}
	stop()
	if stops != 1 {
		t.Fatalf("watch stopped %d times in all, want 1", stops)
	}
}

// Without a signal, stop ends the watch and cancels the context.
func TestPlanningInterruptStopWithoutSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	stops := 0
	env := Env{Interrupts: func() (<-chan os.Signal, func()) { return signals, func() { stops++ } }}
	ctx, stop := interruptibleContext(env, &bytes.Buffer{})
	if ctx.Err() != nil {
		t.Fatal("cancelled before any signal")
	}
	stop()
	if ctx.Err() == nil || stops != 1 {
		t.Fatalf("err %v, stops %d", ctx.Err(), stops)
	}
}

// B-24: every backfill command, history, undo, and --dry-run included,
// first sweeps copies of Cursor's database a killed process left behind.
func TestEveryBackfillCommandSweepsStaleSnapshots(t *testing.T) {
	f := newBackfillFixture(t)
	root := filepath.Join(cursorstore.SnapshotTempDirForTesting, fmt.Sprintf("agent-archive-cursor-%d", os.Getuid()))
	for _, args := range [][]string{{"backfill", "history"}, {"backfill", "undo", "--yes"}, {"backfill", "--dry-run", "--json"}} {
		if _, err := cursorstore.SnapshotRoot(); err != nil {
			t.Fatal(err)
		}
		stale := filepath.Join(root, "cursor-snapshot-stale")
		if err := os.MkdirAll(stale, 0o700); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(stale, "in-use.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
		f.command(t, args...)
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("%v left a stale copy: %v", args, err)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}
