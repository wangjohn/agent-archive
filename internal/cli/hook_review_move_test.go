package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// For a configuration that never recorded its hook files, the move warning
// says an earlier release used the fixed path; it does not claim the
// variable changed since setup last ran.
func TestSetupReviewMoveWarningForUnrecordedHookFiles(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg, _, _ := config.Load(home)
	cfg.HookFiles = nil
	must(t, config.Save(home, cfg))
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\n3\n"), &out, &errOut, env)
	if !strings.Contains(out.String(), "An earlier release installed them at the fixed path") || strings.Contains(out.String(), "differs from when setup last ran") {
		t.Fatalf("review:\n%s%s", &out, &errOut)
	}
}
