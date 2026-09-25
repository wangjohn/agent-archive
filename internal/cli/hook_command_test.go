package cli

import (
	"bytes"
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
