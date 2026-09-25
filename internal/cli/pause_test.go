package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestPauseBusyMakesNoFalseClaimAndDoesNotLoseConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/project", time.Now())
	env := testEnv(t, home, time.Now())
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"pause"}, nil, &out, &errOut, env); code != 1 {
		t.Fatal(code)
	}
	cfg, _, _ := config.Load(home)
	if cfg.Paused {
		t.Fatal("busy pause changed config")
	}
	unlock()
	if code := Run([]string{"pause"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(code)
	}
	cfg, _, _ = config.Load(home)
	if !cfg.Paused {
		t.Fatal("pause not persisted")
	}
}

func TestPauseBlocksSyncAndResumeUnblocksIt(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := testEnv(t, home, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))

	var out, errOut bytes.Buffer
	if code := runPauseCommand(&out, &errOut, env, true); code != 0 {
		t.Fatalf("code=%d", code)
	}

	// A sync that did no work says why on stderr and exits 1, like every
	// command that did not do what it was asked.
	out.Reset()
	errOut.Reset()
	if code := runSyncCommand(nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("paused sync: code=%d stdout=%s", code, out.String())
	}
	if !strings.Contains(errOut.String(), "paused") {
		t.Fatalf("sync should say it's paused, not just do nothing silently: %s", errOut.String())
	}

	out.Reset()
	if code := runPauseCommand(&out, &errOut, env, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.Paused {
		t.Fatalf("cfg=%#v found=%v err=%v", cfg, found, err)
	}
}
