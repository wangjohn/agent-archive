package cli

import (
	"os"
	"testing"
	"time"
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
