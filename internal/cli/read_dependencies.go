package cli

import (
	"io"
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
	isTerminal(any) bool
}

type listCommandDependencies interface {
	readOnlyStoreDependencies
	pagerDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showCommandDependencies interface {
	readOnlyStoreDependencies
	sessionBrowseDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type showQueryDependencies interface {
	metadataCacheDependencies
	sessionBrowseDependencies
	now() time.Time
}

type pagerDependencies interface {
	isTerminal(any) bool
	lookupEnv(string) (string, bool)
	runPager(string, io.Reader, io.Writer, io.Writer) error
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
}
