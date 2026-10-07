package cursor

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// OpenAdmissionPass permits verified files and bounded settled read-only DB
// materialization. It never invokes the ordinary SQLite backup reader.
func (p SourceProvider) OpenAdmissionPass(ctx context.Context, e agentapi.SourceEnvironment, ref agentapi.SourceRef) (agentapi.SourcePass, error) {
	if _, err := p.Describe(ref); err != nil {
		return nil, err
	}
	if ref.Kind == archive.SourceKindFile {
		return sourceio.FileProvider{}.OpenPass(ctx, e)
	}
	pass, err := p.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return &admissionPass{SourcePass: pass, budget: &admissionBudget{rows: 65536, bytes: 128 << 20}}, nil
}

type admissionPass struct {
	agentapi.SourcePass
	budget *admissionBudget
}

func (p *admissionPass) Read(ctx context.Context, ref agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	bounded, ok := p.SourcePass.(agentapi.RecoverySourcePass)
	if !ok {
		return nil, errors.New("bounded settled Cursor admission unavailable")
	}
	budget := p.budget
	if budget == nil {
		return nil, errors.New("cursor admission requires a shared payload budget")
	}
	snapshot, err := bounded.ReadRecovery(ctx, ref, l, budget)
	if err != nil {
		return nil, errors.Join(errors.New("cursor import remains unadmitted; settle the database or retry with bounded snapshot support"), err)
	}
	chatSnapshot, ok := snapshot.(*chatSnapshot)
	if !ok {
		return nil, errors.Join(errors.New("cursor admission snapshot facts unavailable"), snapshot.Close())
	}
	native, ok := decodeComposerData("composerData:"+ref.Key, chatSnapshot.composer.Composer)
	if !ok || !native.counted || native.newer || native.chat.Malformed || native.chat.KeyID != ref.Key || native.chat.ID != ref.Key || native.chat.CreatedAt.IsZero() {
		return nil, errors.Join(errors.New("cursor admission identity or eligibility is unverified"), snapshot.Close())
	}
	return &admissionSnapshot{SourceSnapshot: snapshot, chat: native.chat}, nil
}

func (p *admissionPass) Signature(context.Context, agentapi.SourceRef) (agentapi.SourceObservation, error) {
	return agentapi.SourceObservation{}, errors.New("admission reads require the complete settled snapshot")
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
