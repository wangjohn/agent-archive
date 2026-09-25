package setupjournal

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// setup's transaction record is read by every command and by the hook
// (internal/capture), so it stays free of the command-line layer: no flags,
// prompts, or terminal output, which internal/cli owns. .golangci.yml's
// depguard rules enforce the same on macOS.
func TestSetupJournalImportBoundary(t *testing.T) {
	t.Parallel()
	_, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/setupjournal")
	importgraph.Forbid(t, "internal/setupjournal (transitively)", all,
		"github.com/wangjohn/agent-archive/internal/cli",
		"github.com/wangjohn/agent-archive/internal/capture",
		"github.com/wangjohn/agent-archive/internal/terminal",
		"golang.org/x/term",
	)
}
