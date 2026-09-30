package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// slowStore blocks every Get until the context is done, like a store on a
// stalled network; it says when the first Get started.
type slowStore struct {
	*storagetest.MemoryStore
	started chan struct{}
	gets    atomic.Int32
}

func (s *slowStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.gets.Add(1) == 1 {
		close(s.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// failingStore fails every Get, like a store that lost its credentials.
type failingStore struct{ *storagetest.MemoryStore }

func (failingStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("synthetic outage")
}

// Ctrl-C while sessions are being read stops the read at once, prints
// nothing (no error message, no half-drawn screen) and exits as the signal
// would have: 130 for SIGINT, 143 for SIGTERM. The signal handler is always
// released.
func TestStatsInterruptStopsTheReadQuietly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sig  os.Signal
		code int
	}{{os.Interrupt, 130}, {syscall.SIGTERM, 143}} {
		env, mem := statsEnv(t)
		publishStatsFixture(t, mem)
		store := &slowStore{MemoryStore: mem, started: make(chan struct{})}
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
		signals := make(chan os.Signal, 1)
		var released atomic.Bool
		env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() { released.Store(true) } }

		out := &statsTerminal{width: 100}
		var errOut bytes.Buffer
		result := make(chan int, 1)
		go func() { result <- Run([]string{"stats"}, nil, out, &errOut, env) }()
		select {
		case <-store.started:
		case <-time.After(10 * time.Second):
			t.Fatal("the read never started")
		}
		signals <- tc.sig
		select {
		case code := <-result:
			if code != tc.code {
				t.Errorf("%v: exit code %d, want %d", tc.sig, code, tc.code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: stats did not stop", tc.sig)
		}
		if out.String() != "" || errOut.String() != "" {
			t.Errorf("%v: printed stdout=%q stderr=%q", tc.sig, out.String(), errOut.String())
		}
		if !released.Load() {
			t.Errorf("%v: the signal handler was not released", tc.sig)
		}
	}
}

// A signal that lands as the read finishes still stops the command: it is
// not swallowed by a read that happened to succeed. The signal here arrives
// at the moment the command stops listening for them, after the read is done.
func TestStatsInterruptThatArrivesWithTheLastReadStillStops(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	signals := make(chan os.Signal, 4)
	env.Interrupts = func() (<-chan os.Signal, func()) {
		return signals, func() {
			select {
			case signals <- os.Interrupt:
			default:
			}
		}
	}
	out, errOut, code := runStats(t, env, 100)
	if code != 130 || out != "" || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want 130 and nothing printed", code, out, errOut)
	}
}

// A store that fails is an error message and exit 1, not mistaken for an
// interrupt.
func TestStatsStoreErrorIsNotAnInterrupt(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return failingStore{mem}, nil }
	env.Interrupts = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} }
	out, errOut, code := runStats(t, env, 100)
	if code != 1 || out != "" || !strings.Contains(errOut, "synthetic outage") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
