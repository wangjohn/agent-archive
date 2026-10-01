package cursor

import (
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"testing"
)

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()
	agenttest.RuntimeConformance(t, RuntimeDetector{}, "CURSOR_AGENT", false, []string{"CURSOR_AGENT"})
}
func TestLaunchConformance(t *testing.T) {
	t.Parallel()
	agenttest.LaunchConformance(t, Launcher{}, []string{"agent", "cursor-agent"}, []string{"--workspace", "/project", "--model", "x", "PROMPT"})
}
