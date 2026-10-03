package fileapply

import (
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
	"testing"
)

func TestFileApplicationHasNoIntegrationDependencies(t *testing.T) {
	t.Parallel()
	_, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/fileapply")
	importgraph.Forbid(t, "generic file application", all,
		"github.com/wangjohn/agent-archive/internal/agentapi",
		"github.com/wangjohn/agent-archive/internal/hooks",
		"github.com/wangjohn/agent-archive/internal/agents/hookconfig",
		"github.com/wangjohn/agent-archive/internal/setupjournal", "os/exec", "net/http")
}
