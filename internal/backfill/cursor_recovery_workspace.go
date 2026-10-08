package backfill

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func recoveryWorkspaceReader(ctx context.Context, env Environment, budget *cursorRecoveryReadBudget, complete *bool, failure *error) func(string) (data []byte, err error) {
	return func(path string) (data []byte, err error) {
		defer func() {
			if err != nil && !isNotExist(err) {
				*complete = false
				if fatalSourceFailure(err) {
					*failure = errors.Join(*failure, err)
				}
			}
		}()
		if env.ReadFile != nil {
			return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("bounded workspace source unavailable"))
		}
		root := cursorWorkspaceStorage(env)
		// The declared storage root and metadata path must not traverse symlinks.
		if env.resolved(root) != filepath.Clean(root) || env.resolved(path) != filepath.Clean(path) {
			return nil, transcriptio.ErrChanged
		}
		s, err := transcriptio.Open(sourcefacts.RootOpener{Root: root}, path, transcriptio.OpenPolicy{Root: root, RejectSymlinks: true})
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, agentapi.Wrap(agentapi.Cleanup, s.Close())) }()
		size := s.Stamp().Size
		if size > 1<<20 {
			budget.exhausted = true
			return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		if err := budget.Charge(0, size); err != nil {
			return nil, err
		}
		data, err = io.ReadAll(s.Reader(ctx))
		if err != nil {
			return nil, err
		}
		return data, s.Check()
	}
}
