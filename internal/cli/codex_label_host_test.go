package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

type countedLabelOutput struct {
	io.Reader
	read *atomic.Int64
}

func (r countedLabelOutput) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read.Add(int64(n))
	return n, err
}

// Unsolicited lines left behind a successful response still spend the pass cap.
func TestCodexNamingBoundsReadAheadAcrossHomes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := &labelStdoutBudget{remaining: 1 << 20}
		var read atomic.Int64
		var hosts []*codexLabelHost
		for range 8 {
			output := "{}\n" + strings.Repeat(strings.Repeat("x", 120<<10)+"\n", 4)
			h := &codexLabelHost{output: io.NopCloser(countedLabelOutput{strings.NewReader(output), &read}), stdoutBudget: budget, lines: make(chan labelLine, 1), done: make(chan struct{})}
			hosts = append(hosts, h)
			go h.readLines()
			_, _ = h.ReadLine(context.Background())
			synctest.Wait()
		}
		if n := read.Load(); n > 1<<20 {
			t.Errorf("unsolicited stdout read across homes = %d, limit %d", n, 1<<20)
		}
		for _, h := range hosts {
			close(h.done)
			_ = h.output.Close()
		}
		synctest.Wait()
	})
}

func TestCodexNamingFreshPassRestoresStdoutAllowance(t *testing.T) {
	env, home := labelHostScript(t, "read line\nprintf '%s\\n' '{\"id\":1,\"result\":{}}'\nread line\n")
	flood := filepath.Join(home, "flood")
	notification := `{"method":"progress","params":"` + strings.Repeat("x", 64<<10) + `"}` + "\n"
	if err := os.WriteFile(flood, []byte("#!/bin/sh\ncat <<'FLOOD'\n"+strings.Repeat(notification, 17)+"FLOOD\nread line\n"), 0700); err != nil {
		t.Fatal(err)
	}
	validPath, _ := env.LookPath("codex")
	lookups := 0
	env.LookPath = func(string) (string, error) {
		lookups++
		if lookups == 1 {
			return flood, nil
		}
		return validPath, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for pass := range 2 {
		// labelProviders composes one of these factories for each collector Run.
		host, err := env.codexLabelHostFactory()(ctx, home)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 {
			readBytes := 0
			for {
				line, err := host.ReadLine(ctx)
				if err != nil {
					break
				}
				readBytes += len(line)
			}
			if ctx.Err() != nil || readBytes < 900<<10 {
				t.Fatalf("first factory did not exhaust actual stdout: %d bytes, %v", readBytes, ctx.Err())
			}
		} else {
			if err := host.WriteLine(ctx, []byte(`{"id":1}`)); err != nil {
				t.Fatal(err)
			}
			line, err := host.ReadLine(ctx)
			if err != nil || string(line) != `{"id":1,"result":{}}` {
				t.Fatalf("fresh pass retained exhausted allowance: %q %v", line, err)
			}
		}
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexNamingExhaustedStdoutClosesAndReapsHost(t *testing.T) {
	env, home := labelHostScript(t, "printf '123456789\\n'\nexec /bin/sleep 30\n")
	host, err := env.startCodexLabelHost(context.Background(), home, &labelStdoutBudget{remaining: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := host.ReadLine(ctx); err == nil {
		t.Fatal("exhausted stdout accepted a partial line")
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.(*codexLabelHost).waited:
	default:
		t.Fatal("exhausted host was not reaped")
	}
}

func TestFilesNamingNeverStartsHost(t *testing.T) {
	for _, mode := range []config.CodexNameLookup{"", config.CodexNameLookupFiles} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.LookPath = func(string) (string, error) {
			t.Fatal("files lookup searched for executable")
			return "", errors.New("forbidden")
		}
		env.CodexLabelHost = func(context.Context, string) (agentapi.LabelTransport, error) {
			t.Fatal("files lookup started native host")
			return nil, errors.New("forbidden")
		}
		provider, ok := env.labelProviders(config.Config{CodexNameLookup: mode}).LookupLabels("codex")
		if !ok {
			t.Fatal("file provider missing")
		}
		home := t.TempDir()
		id := "01900000-0000-7000-8000-000000000001"
		request := agentapi.LabelRequest{Registration: archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: id, Harness: archive.Harness{Name: "codex"}, TranscriptPath: filepath.Join(home, "sessions", "a.jsonl"), DiscoveryRoot: home}, Context: agentapi.LabelContext{NativeID: id, Ordinary: true, APICompatible: true, Producer: "0.159.2", Contract: "test"}}
		_ = provider.LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{home}}, []agentapi.LabelRequest{request})
	}
}

func TestCodexNamingChildEnvironmentExcludesArchiveCredentials(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	values := map[string]string{"AWS_SECRET_ACCESS_KEY": "private", "OPENAI_API_KEY": "private", "CODEX_SQLITE_HOME": "/external", "PATH": "/safe/bin", "LANG": "C", "CODEX_HOME": "/wrong"}
	env.LookupEnv = func(key string) (string, bool) { v, ok := values[key]; return v, ok }
	got := env.codexLabelEnvironment("/approved")
	if got[0] != "CODEX_HOME=/approved" || got[1] != "CODEX_SQLITE_HOME=/approved" {
		t.Fatalf("home: %v", got)
	}
	for _, entry := range got {
		if strings.Contains(entry, "private") || strings.Contains(entry, "external") || strings.Contains(entry, "wrong") {
			t.Fatalf("unsafe environment %q", entry)
		}
	}
}

func TestCodexNamingHostMissingExecutableIsUnavailable(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.LookPath = func(name string) (string, error) {
		if name != "codex" {
			t.Fatal(name)
		}
		return "", errors.New("private path")
	}
	_, err := env.startCodexLabelHost(context.Background(), t.TempDir(), nil)
	if !errors.Is(err, agentapi.ErrLabelHostMissing) || strings.Contains(err.Error(), "private") {
		t.Fatalf("error: %v", err)
	}
}

func labelHostScript(t *testing.T, body string) (Env, string) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "host")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, t.TempDir(), time.Now())
	env.LookPath = func(string) (string, error) { return path, nil }
	return env, home
}

func TestCodexNamingProcessWireAndReap(t *testing.T) {
	env, home := labelHostScript(t, "read line\nprintf '%s\\n' '{\"id\":1,\"result\":{}}'\nread line\n")
	ctx := context.Background()
	transport, err := env.startCodexLabelHost(ctx, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteLine(ctx, []byte(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	b, err := transport.ReadLine(ctx)
	if err != nil || string(b) != `{"id":1,"result":{}}` {
		t.Fatalf("read %s %v", b, err)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.(*codexLabelHost).waited:
	default:
		t.Fatal("host not reaped")
	}
}

func TestCodexNamingCancellationKillsAndReaps(t *testing.T) {
	env, home := labelHostScript(t, "exec /bin/sleep 30\n")
	transport, err := env.startCodexLabelHost(context.Background(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.ReadLine(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.(*codexLabelHost).waited:
	default:
		t.Fatal("cancelled host not reaped")
	}
}

func TestCodexNamingStreamRejectsPartialAndOversizedLines(t *testing.T) {
	for _, body := range []string{"printf '{'\n", "/bin/dd if=/dev/zero bs=262145 count=1 2>/dev/null\n"} {
		env, home := labelHostScript(t, body)
		ctx := context.Background()
		h, err := env.startCodexLabelHost(ctx, home, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.ReadLine(ctx)
		if err == nil {
			t.Fatal("invalid stream accepted")
		}
		_ = h.Close()
	}
}

func TestCodexNamingStderrIsDiscardedAndBounded(t *testing.T) {
	calls := 0
	w := labelDiscard{limit: 16 << 10, exceeded: func() { calls++ }}
	n, err := w.Write(make([]byte, 16<<10))
	if n != 16<<10 || err != nil {
		t.Fatal(n, err)
	}
	n, err = w.Write([]byte("private"))
	if n != 7 || err == nil || calls != 1 {
		t.Fatal(n, err, calls)
	}
}

func TestNativeNamingPreferenceDoesNotLaunchForListOrShow(t *testing.T) {
	fixture := newScopedArchive(t)
	home, err := fixture.env.Home()
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := config.Load(home)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	cfg.CodexNameLookup = config.CodexNameLookupNative
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	fixture.env.CodexLabelHost = func(context.Context, string) (agentapi.LabelTransport, error) {
		t.Fatal("archive reader started naming host")
		return nil, errors.New("forbidden")
	}
	for _, args := range [][]string{{"list", "--json"}, {"show", fixture.id, "--harness", "codex"}} {
		var output, diagnostics bytes.Buffer
		if code := Run(args, nil, &output, &diagnostics, fixture.env); code != 0 {
			t.Fatalf("%v: %d %s", args, code, diagnostics.String())
		}
	}
}

func TestCodexNamingExecutableSearchHonorsCancellation(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	env.LookPath = func(string) (string, error) {
		close(entered)
		<-release
		close(returned)
		return "", errors.New("unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := env.labelExecutable(ctx); result <- err }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	<-returned
}
