package setupjournal

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

// setup's transaction record is read by every command and by the hook
// (internal/capture), so it stays free of the command-line layer: no flags,
// prompts, or terminal output, which internal/cli owns. It drives launchd
// only through the Launchd its caller passes, so it runs no program itself.
// .golangci.yml's depguard rules enforce the same on macOS.
func TestSetupJournalImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/setupjournal")
	importgraph.Forbid(t, "internal/setupjournal (transitively)", all,
		"github.com/wangjohn/agent-archive/internal/cli",
		"github.com/wangjohn/agent-archive/internal/capture",
		"github.com/wangjohn/agent-archive/internal/terminal",
		"golang.org/x/term",
	)
	importgraph.Forbid(t, "internal/setupjournal", direct,
		"os/exec",
		"os/signal",
		"github.com/wangjohn/agent-archive/internal/credentials",
	)
}
