package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This stand-in deliberately has no Env field. A helper that starts using a
// setup or collector capability must declare that dependency explicitly.
type readStoreStub struct {
	home  string
	store storage.ObjectStore
	calls int
}

func (s *readStoreStub) readHome() (string, error) { return s.home, nil }

func (s *readStoreStub) openStore(cfg config.Config) (storage.ObjectStore, error) {
	s.calls++
	if cfg.MachineID != "reader-test" {
		return nil, errors.New("wrong config")
	}
	return s.store, nil
}

func TestReadOnlyStoreBoundary(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	stub := &readStoreStub{home: home, store: storagetest.NewMemoryStore()}
	_, _, found, err := openReadOnlyStore(stub)
	if err != nil || found || stub.calls != 0 {
		t.Fatalf("before setup: found=%v calls=%d err=%v", found, stub.calls, err)
	}
	if err := config.Save(home, config.Config{MachineID: "reader-test"}); err != nil {
		t.Fatal(err)
	}
	store, cfg, found, err := openReadOnlyStore(stub)
	if err != nil || !found || store != stub.store || cfg.MachineID != "reader-test" || stub.calls != 1 {
		t.Fatalf("after setup: store=%v found=%v cfg=%+v calls=%d err=%v", store, found, cfg, stub.calls, err)
	}
}

type pagerStub struct {
	run int
}

func (*pagerStub) interactive(any) bool { return true }

func (*pagerStub) lookupEnv(string) (string, bool) { return "more", true }

func (*pagerStub) lessVersion(string) (int, bool) { return 0, false }

func (*pagerStub) interrupts() (<-chan os.Signal, func()) { return nil, func() {} }

func (*pagerStub) exit(int) {}

func (s *pagerStub) runPager(_ context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
	s.run++
	return errors.New("pager unavailable")
}

func TestPagerBoundaryFallsBackToDirectOutput(t *testing.T) {
	t.Parallel()
	deps := &pagerStub{}
	var out, errOut bytes.Buffer
	err := withPager(context.Background(), &out, &errOut, deps, false, func(w io.Writer) error {
		_, err := io.WriteString(w, "metadata only\n")
		return err
	})
	if err != nil || out.String() != "metadata only\n" || deps.run != 1 || !strings.Contains(errOut.String(), "pager") {
		t.Fatalf("out=%q stderr=%q calls=%d err=%v", out.String(), errOut.String(), deps.run, err)
	}
}

type sessionStub struct{}

func (sessionStub) lookupEnv(key string) (string, bool) {
	if key == "CODEX_THREAD_ID" {
		return "  current-session  ", true
	}
	return "", false
}

func TestCurrentSessionBoundaryTrimsIdentifier(t *testing.T) {
	t.Parallel()
	ids := currentSessions(sessionStub{})
	if len(ids) != 1 || !ids["current-session"] {
		t.Fatalf("ids=%v", ids)
	}
}

type resolverStub struct {
	store storage.ObjectStore
	calls int
}

func (*resolverStub) readHome() (string, error) { return "", nil }

func (s *resolverStub) openStore(config.Config) (storage.ObjectStore, error) {
	s.calls++
	return s.store, nil
}

func (*resolverStub) now() time.Time { return time.Unix(1, 0) }

func (*resolverStub) cursorDatabase() string { return "" }

func (*resolverStub) repoKeyResolver() func(string) string { return func(string) string { return "" } }

func TestHandoffResolverUsesReadStoreBoundary(t *testing.T) {
	t.Parallel()
	_, mem, id := publishedFixture(t)
	deps := &resolverStub{store: mem}
	target, err := (handoffResolver{ctx: context.Background(), env: deps, source: "archive"}).byID(id)
	if err != nil || target.source != "archive" || target.metadata == nil || target.metadata.SessionID != id || deps.calls != 1 {
		t.Fatalf("target=%+v calls=%d err=%v", target, deps.calls, err)
	}
}

var _ handoffFileDependencies = (*resolverStub)(nil)
