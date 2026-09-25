package capture

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

// The hook runtime's import boundary, which .golangci.yml's depguard rules
// also enforce on macOS. It runs on the user's turn, so it must stay free of
// the command-line layer (flags, prompts, terminal output: internal/cli owns
// those and the exit code), and must not reach anything a hook must never do:
// run launchctl or any other program, open the Keychain or a bucket, or use
// the network. The transitive check covers what no import may bring in; the
// direct one covers what capture must not call itself (config, for one,
// imports storage for its types).
func TestCaptureImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/capture")
	importgraph.Forbid(t, "internal/capture (transitively)", all,
		"github.com/wangjohn/agent-archive/internal/cli",
		"github.com/wangjohn/agent-archive/internal/terminal",
		"golang.org/x/term",
	)
	importgraph.Forbid(t, "internal/capture", direct,
		"os/exec",
		"os/signal",
		"net",
		"net/http",
		"github.com/wangjohn/agent-archive/internal/credentials",
		"github.com/wangjohn/agent-archive/internal/storage",
		"github.com/wangjohn/agent-archive/internal/collector",
		"github.com/wangjohn/agent-archive/internal/hooks",
	)
}
