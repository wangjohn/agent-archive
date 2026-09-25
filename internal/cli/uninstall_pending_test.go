package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func mustConfig(t *testing.T, home string) config.Config {
	t.Helper()
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config: found=%v err=%v", found, err)
	}
	return cfg
}

// startSession sends the hook events of one Claude Code session in project,
// as the app would: the session registers and has work outstanding.
func startSession(t *testing.T, env Env, project, id string) {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), id+".jsonl")
	must(t, os.WriteFile(transcript, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600))
	for _, event := range []string{"SessionStart", "Stop"} {
		payload := `{"hook_event_name":"` + event + `","session_id":"` + id + `","source":"startup","cwd":"` + project + `","transcript_path":"` + transcript + `"}`
		var errOut bytes.Buffer
		if code := runHookCommand([]string{"--harness", "claude"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() > 0 {
			t.Fatalf("hook %s: exit %d %s", event, code, &errOut)
		}
	}
}

// onRead runs hook once, when the second line starts being read:
// here, when uninstall reads the answer to its second question.
type onRead struct {
	r     io.Reader
	reads int
	hook  func()
}

func (o *onRead) Read(p []byte) (int, error) {
	o.reads++
	if o.reads == 3 && o.hook != nil {
		o.hook()
		o.hook = nil
	}
	// One byte at a time, so each question reads its own answer.
	if len(p) > 1 {
		p = p[:1]
	}
	return o.r.Read(p)
}

// H-21: the data-loss warning of uninstall --delete-local-data counts the
// sessions status reports as pending (it used a zero configuration, which
// admits no registration, so it always said 0), and a session that registers
// while the person is confirming stops the purge.
func TestUninstallPurgeCountsPendingSessions(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	startSession(t, env, project, "sess-1")
	if n, err := pendingSessions(home, mustConfig(t, home)); err != nil || n != 1 {
		t.Fatalf("pending = %d, %v", n, err)
	}
	var out, errOut bytes.Buffer
	code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\nn\n"), &out, &errOut, env)
	if code != 0 || !strings.Contains(out.String(), "1 pending session(s)") {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}

	// A second session arrives between the preview and the purge.
	stdin := &onRead{r: strings.NewReader("y\ny\n")}
	stdin.hook = func() { startSession(t, env, project, "sess-2") }
	out.Reset()
	errOut.Reset()
	env.IsTerminal = func(any) bool { return true }
	code = runUninstallCommand([]string{"--delete-local-data"}, stdin, &out, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), "new pending evidence appeared") {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "registrations")); err != nil {
		t.Fatalf("the purge went ahead: %v", err)
	}
}

// Without a configuration nothing says which sessions would be uploaded, so
// every registration with work outstanding counts.
func TestUnpublishedSessionsWithoutConfigurationCountsAll(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	startSession(t, env, project, "sess-1")
	cfg := mustConfig(t, home)
	must(t, os.Remove(filepath.Join(home, "config.json")))
	if n, _, err := unpublishedSessions(home, cfg, false); err != nil || n != 1 {
		t.Fatalf("unpublished = %d, %v", n, err)
	}
}
