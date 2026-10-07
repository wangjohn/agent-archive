package cli

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexNamingStderrReadBudgetAcrossHomes(t *testing.T) {
	var read atomic.Int64
	budget := &labelStreamBudget{stderrRemaining: 16 << 10}
	for range 2 {
		output := countedLabelOutput{strings.NewReader(strings.Repeat("x", 32<<10)), &read}
		_ = discardLabelStderr(output, budget)
	}
	if got := read.Load(); got > 16<<10 {
		t.Fatalf("stderr pipe bytes across homes = %d, limit %d", got, 16<<10)
	}
}

func TestCodexNamingStderrExhaustionKillsAndDrainsHost(t *testing.T) {
	env, home := labelHostScript(t, "/bin/dd if=/dev/zero bs=32768 count=1 1>&2 2>/dev/null\nexec /bin/sleep 30\n")
	budget := &labelStreamBudget{remaining: 1 << 20, stderrRemaining: 16 << 10}
	transport, err := env.startCodexLabelHost(context.Background(), home, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Close() }()
	host := transport.(*codexLabelHost)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-host.stderrDone:
	case <-ctx.Done():
		t.Fatal("stderr flood was not stopped")
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.stderrRemaining != 0 {
		t.Fatalf("stderr allowance not exhausted: %d", budget.stderrRemaining)
	}
	for _, done := range []chan struct{}{host.waited, host.stdoutDone, host.stderrDone} {
		select {
		case <-done:
		default:
			t.Fatal("native host retained an owned reader or reap")
		}
	}
}
