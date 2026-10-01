package backfill

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// CursorStateDatabase is where Cursor keeps its chats under home on this
// machine (see platform.Locations).
func CursorStateDatabase(home string) string {
	return cursorstore.StateDatabase(home)
}

// CursorDatabaseReader is CursorDatabaseReaderFor for the state.vscdb under
// home on this machine.
func CursorDatabaseReader(home string) func(context.Context) (CursorDatabaseResult, error) {
	return CursorDatabaseReaderFor(Environment{NativeHeaders: builtin.NewBuiltins(), Home: home})
}
