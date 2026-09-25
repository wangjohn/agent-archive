package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
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
