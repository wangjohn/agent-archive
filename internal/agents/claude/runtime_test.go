package claude

import (
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"testing"
)

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()
	agenttest.RuntimeConformance(t, RuntimeDetector{}, "CLAUDE_CODE_SESSION_ID", true, []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_PID", "CLAUDE_EFFORT", "AI_AGENT"})
}
func TestLaunchConformance(t *testing.T) {
	t.Parallel()
	agenttest.LaunchConformance(t, Launcher{}, []string{"claude"}, []string{"--add-dir", "/private", "--model", "x", "--", "PROMPT"})
}
