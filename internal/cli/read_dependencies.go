package cli

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Run passes Env at the command boundary. The read commands and their
// helpers declare only the capabilities they use; Env supplies the defaults.
type readOnlyStoreDependencies interface {
	readHome() (string, error)
	openStore(config.Config) (storage.ObjectStore, error)
}

type metadataCacheDependencies interface {
	readHome() (string, error)
}

type sessionBrowseDependencies interface {
	interactive(any) bool
}

type sessionSelectionDependencies interface {
	metadataCacheDependencies
	now() time.Time
}

// sessionBrowserDependencies is what the interactive session browser uses:
// the pager for transcripts, and interrupts so it can restore the screen
// before it exits.
type sessionBrowserDependencies interface {
	pagerDependencies
	interrupts() (<-chan os.Signal, func())
	exit(int)
}

type listCommandDependencies interface {
	readOnlyStoreDependencies
	sessionBrowserDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

// statsCommandDependencies is what `stats` uses: list's store and cache, the
// pager, and the environment for the character set.
type statsCommandDependencies interface {
	readOnlyStoreDependencies
	pagerDependencies
	// isTerminal is whether stdout is a terminal, whatever the agent switch
	// says: stats --html never fills one with markup.
	isTerminal(any) bool
	interrupts() (<-chan os.Signal, func())
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showCommandDependencies interface {
	readOnlyStoreDependencies
	sessionBrowserDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showQueryDependencies interface {
	metadataCacheDependencies
	sessionBrowseDependencies
	now() time.Time
}

type pagerDependencies interface {
	interactive(any) bool
	lookupEnv(string) (string, bool)
	runPager(context.Context, string, io.Reader, io.Writer, io.Writer) error
}

type handoffResolverDependencies interface {
	readHome() (string, error)
	openStore(config.Config) (storage.ObjectStore, error)
	now() time.Time
	cursorDatabase() string
}

type handoffFileDependencies interface {
	now() time.Time
}

type currentSessionDependencies interface {
	lookupEnv(string) (string, bool)
}

type handoffOptionsDependencies interface {
	newCommandFlags(string, io.Writer) *commandFlags
}

type handoffTargetDependencies interface {
	handoffResolverDependencies
	currentSessionDependencies
	workingDir() (string, error)
}

type handoffCommandDependencies interface {
	handoffOptionsDependencies
	handoffTargetDependencies
	sessionBrowseDependencies
	executable() (string, error)
	tempDir() string
	launchHandoff(string, string, string, io.Reader, io.Writer, io.Writer) error
}
