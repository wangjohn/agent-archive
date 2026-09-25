package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHookCommandNeverFailsOnMalformedInput(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var errOut bytes.Buffer
	code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader("not json"), &errOut, env)
	if code != 0 {
		t.Fatalf("hook must never fail the harness's turn: code=%d stderr=%s", code, errOut.String())
	}
}

// A hook left in an app's settings after `uninstall --delete-local-data`
// does nothing, quietly, and does not bring the data directory back.
func TestRunHookCommandLeavesADeletedDataDirectoryDeleted(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "agent-archive")
	env := testEnv(t, home, time.Now())
	payload := `{"hook_event_name":"SessionStart","session_id":"s1","source":"startup"}`
	var errOut bytes.Buffer
	if code := runHookCommand([]string{"--harness", "claude"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() > 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hook recreated the data directory: %v", err)
	}
}
