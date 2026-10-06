package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

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
	_, err := env.startCodexLabelHost(context.Background(), t.TempDir())
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
	transport, err := env.startCodexLabelHost(ctx, home)
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
	transport, err := env.startCodexLabelHost(context.Background(), home)
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
		h, err := env.startCodexLabelHost(ctx, home)
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
