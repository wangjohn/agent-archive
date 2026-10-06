package cursor

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// ReadRecovery supplies optional bounded settled database evidence through the
// same native records/privacy filter, without snapshots or admission signatures.
func (p *sourcePass) ReadRecovery(ctx context.Context, r agentapi.SourceRef, l agentapi.ReadLimits, budget agentapi.RecoveryReadBudget) (agentapi.SourceSnapshot, error) {
	if _, err := (SourceProvider{}).Describe(r); err != nil {
		return nil, agentapi.Wrap(agentapi.Unsafe, err)
	}
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if r.Kind != archive.SourceKindCursorSQLite {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("unsupported recovery source"))
	}
	c, err := cursorstore.ReadRecoveryComposer(ctx, p.database, r.Key, l.RecordBytes, budget)
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	// No source observation is an admission proof. Recovery's private epoch binds
	// complete eligibility/ownership facts to the settled file/side observations.
	s := &chatSnapshot{owner: p, composer: c}
	p.live[s] = true
	return s, nil
}
