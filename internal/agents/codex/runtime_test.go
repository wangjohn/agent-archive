package codex

import (
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"testing"
)

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()
	agenttest.RuntimeConformance(t, RuntimeDetector{}, "CODEX_THREAD_ID", true, []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_CI", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_PERMISSION_PROFILE", "CODEX_VERSION"})
}
func TestLaunchConformance(t *testing.T) {
	t.Parallel()
	agenttest.LaunchConformance(t, Launcher{}, []string{"codex"}, []string{"--cd", "/project", "--model", "x", "--", "PROMPT"})
}
