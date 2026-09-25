package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
)

// A panic anywhere in the hook handler must still exit 0: Go's own exit 2
// would block the user's prompt in Claude Code. The failure is reported on
// stderr and left as a content-free diagnostic for status.
func TestHookRecoversFromAPanicAndExitsZero(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := testEnv(t, home, time.Now())
	env.Now = func() time.Time { panic("injected failure") }
	payload := `{"hook_event_name":"UserPromptSubmit","session_id":"native-1","cwd":"` + project + `","prompt":"private words"}`
	var errOut bytes.Buffer
	if code := Run([]string{"_hook", "--harness", "claude"}, strings.NewReader(payload), nil, &errOut, env); code != 0 {
		t.Fatalf("hook exit %d after a panic", code)
	}
	if !strings.Contains(errOut.String(), "internal error: injected failure") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	ds, err := capture.ReadDiagnostics(home)
	if err != nil || len(ds) != 1 || ds[0].Code != capture.DiagnosticHookFailed || ds[0].ProjectRoot != project || ds[0].Harness != "claude" {
		t.Fatalf("diagnostics=%#v err=%v", ds, err)
	}
	if raw, _ := os.ReadFile(capture.DiagnosticsPath(home)); bytes.Contains(raw, []byte("private")) || bytes.Contains(raw, []byte("native-1")) {
		t.Fatalf("diagnostic leaked content: %s", raw)
	}
}
