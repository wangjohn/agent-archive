package cursor

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// OpenAdmissionPass permits verified files and bounded immutable SQLite input.
// Live databases require caller-reserved scratch and the admission-only backup;
// ordinary OpenPass and its snapshot policy remain unchanged.
func (p SourceProvider) OpenAdmissionPass(ctx context.Context, e agentapi.SourceEnvironment, ref agentapi.SourceRef) (agentapi.SourcePass, error) {
	if _, err := p.Describe(ref); err != nil {
		return nil, err
	}
	if ref.Kind == archive.SourceKindFile {
		return sourceio.FileProvider{}.OpenAdmissionPass(ctx, e, ref)
	}
	pass, err := p.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return &admissionPass{SourcePass: pass, environment: e, ref: ref, budget: &admissionBudget{rows: 65536, bytes: 128 << 20}}, nil
}

type admissionPass struct {
	agentapi.SourcePass
	budget       *admissionBudget
	environment  agentapi.SourceEnvironment
	ref          agentapi.SourceRef
	prepared     string
	preparedLive bool
	failed       error
}

func (p *admissionPass) Read(ctx context.Context, ref agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref != p.ref {
		return nil, errors.New("cursor admission source binding changed")
	}
	if p.budget == nil {
		return nil, errors.New("cursor admission requires a shared payload budget")
	}
	if p.failed != nil {
		return nil, p.failed
	}
	owner, ok := p.SourcePass.(*sourcePass)
	if !ok || owner.closed {
		return nil, agentapi.ErrClosed
	}
	if p.prepared == "" {
		workspace, _ := p.environment.TemporaryBytes.(agentapi.TemporaryWorkspaceBudget)
		path, stats, err := cursorstore.PrepareAdmissionDatabase(ctx, owner.database, workspace)
		if err != nil {
			p.failed = err
			return nil, err
		}
		p.prepared = path
		p.preparedLive = stats.WorkspaceImage
	}
	var liveWorkspace []agentapi.TemporaryWorkspaceBudget
	if p.preparedLive {
		workspace, _ := p.environment.TemporaryBytes.(agentapi.TemporaryWorkspaceBudget)
		liveWorkspace = append(liveWorkspace, workspace)
	}
	c, err := cursorstore.ReadAdmissionComposer(ctx, p.prepared, ref.Key, l.RawBytes, l.RecordBytes, p.budget, func(ctx context.Context, host agentapi.DatabaseCatalogHost) error {
		return host.Query(ctx, cursorComposerQuery, func(record agentapi.DatabaseRecord) error {
			d, ok := decodeComposerData(record.Key, record.Value)
			if !ok || d.newer || d.chat.CursorFacts.Relationships == agentapi.CursorRelationshipsMalformed {
				return errors.New("cursor admission native routing is unverified")
			}
			for _, child := range d.subagents {
				if child == ref.Key {
					return errors.New("cursor admission selected composer is now a native child; review again")
				}
			}
			return nil
		})
	}, liveWorkspace...)
	if err != nil {
		p.failed = errors.Join(errors.New("cursor import remains unadmitted; review changed input or settle the database and retry"), err)
		return nil, p.failed
	}
	chatSnapshot := &chatSnapshot{owner: owner, composer: c}
	owner.live[chatSnapshot] = true
	snapshot := agentapi.SourceSnapshot(chatSnapshot)
	native, ok := decodeComposerData("composerData:"+ref.Key, chatSnapshot.composer.Composer)
	if !ok || !native.counted || native.newer || native.chat.Malformed || native.chat.KeyID != ref.Key || native.chat.ID != ref.Key || native.chat.CreatedAt.IsZero() || native.chat.CursorFacts.Relationships == agentapi.CursorRelationshipsMalformed {
		return nil, errors.Join(errors.New("cursor admission identity or eligibility is unverified"), snapshot.Close())
	}
	return &admissionSnapshot{SourceSnapshot: snapshot, chat: native.chat}, nil
}

func (p *admissionPass) Signature(context.Context, agentapi.SourceRef) (agentapi.SourceObservation, error) {
	return agentapi.SourceObservation{}, errors.New("admission reads require the complete immutable snapshot")
}

type admissionBudget struct {
	rows  int
	bytes int64
}

func (b *admissionBudget) RemainingRows() int { return b.rows }

func (b *admissionBudget) RemainingBytes() int64 { return b.bytes }

func (b *admissionBudget) Charge(rows int, bytes int64) error {
	if rows < 0 || bytes < 0 || rows > b.rows || bytes > b.bytes {
		return errors.New("cursor admission evidence capacity exhausted")
	}
	b.rows -= rows
	b.bytes -= bytes
	return nil
}

type admissionSnapshot struct {
	agentapi.SourceSnapshot
	chat agentapi.DatabaseChat
}

func (s *admissionSnapshot) AdmissionChat() agentapi.DatabaseChat { return s.chat }
