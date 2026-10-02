package cursor

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"time"
)

// SourceProvider supports Cursor files and one lazy database reader per serial pass.
type SourceProvider struct{}

// Describe validates the locator and declares its consistency policy.
func (SourceProvider) Describe(r agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	if r.Kind == archive.SourceKindFile {
		return sourceio.FileProvider{}.Describe(r)
	}
	if r.Kind != archive.SourceKindCursorSQLite || r.Key == "" || r.Path != "" {
		return agentapi.SourceSemantics{}, errors.New("unsupported Cursor source")
	}
	return agentapi.SourceSemantics{Mutation: agentapi.ReplaceableSnapshot, Provider: "cursor/sqlite"}, nil
}

// OpenPass creates a lazy serial owner without opening source content.
func (SourceProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	file, err := (sourceio.FileProvider{}).OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return &sourcePass{file: file, reader: cursorstore.NewReader(e.Database), database: e.Database, live: map[*chatSnapshot]bool{}, signature: cursorstore.ReadSignature}, nil
}

type sourcePass struct {
	file        agentapi.SourcePass
	reader      *cursorstore.Reader
	database    string
	live        map[*chatSnapshot]bool
	closed      bool
	closeErr    error
	signature   func(context.Context, string, string) (cursorstore.Signature, error)
	lastChecked string
}

func (p *sourcePass) Signature(ctx context.Context, r agentapi.SourceRef) (agentapi.SourceObservation, error) {
	if _, err := (SourceProvider{}).Describe(r); err != nil {
		return agentapi.SourceObservation{}, agentapi.Wrap(agentapi.Unsafe, err)
	}
	if p.closed {
		return agentapi.SourceObservation{}, agentapi.ErrClosed
	}
	if r.Kind == archive.SourceKindFile {
		return p.file.Signature(ctx, r)
	}
	p.lastChecked = ""
	sig, err := p.signature(ctx, p.database, r.Key)
	if err != nil {
		return agentapi.SourceObservation{}, sourceio.Classify(err)
	}
	p.lastChecked = r.Key
	return chatObservation(sig), nil
}
func chatObservation(sig cursorstore.Signature) agentapi.SourceObservation {
	at := time.Time{}
	if sig.LastUpdatedAt > 0 {
		at = time.UnixMilli(sig.LastUpdatedAt)
	}
	return agentapi.SourceObservation{Signature: sourceio.CursorSignature(sig), Present: true, Empty: sig.HeaderCount == 0, Activity: at}
}
func (p *sourcePass) Read(ctx context.Context, r agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if _, err := (SourceProvider{}).Describe(r); err != nil {
		return nil, agentapi.Wrap(agentapi.Unsafe, err)
	}
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if r.Kind == archive.SourceKindFile {
		return p.file.Read(ctx, r, l)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.lastChecked != r.Key {
		if _, err := p.signature(ctx, p.database, r.Key); err != nil {
			return nil, sourceio.Classify(err)
		}
	}
	// Admission of this live key is not snapshot evidence. Read and limit failures
	// always carry the actual checked snapshot's own signature.
	p.lastChecked = ""
	c, sig, err := p.reader.ReadComposerLimited(ctx, r.Key, l.RawBytes, l.RecordBytes)
	if errors.Is(err, cursorstore.ErrComposerNotFound) {
		err = cursorstore.NotChecked(cursorstore.ChangedDuringRead)
	}
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	s := &chatSnapshot{owner: p, composer: c, observed: chatObservation(sig)}
	p.live[s] = true
	return s, nil
}
func (p *sourcePass) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	for s := range p.live {
		p.closeErr = errors.Join(p.closeErr, s.Close())
	}
	p.closeErr = errors.Join(p.closeErr, p.file.Close(), agentapi.Wrap(agentapi.Cleanup, p.reader.Close()))
	return p.closeErr
}

// Snapshots retains the collector's structural observation without opening sources.
func (p *sourcePass) Snapshots() int { return p.reader.Snapshots() }

type chatSnapshot struct {
	owner    *sourcePass
	composer cursorstore.Composer
	observed agentapi.SourceObservation
	closed   bool
}

func (s *chatSnapshot) Observation() agentapi.SourceObservation { return s.observed }
func (s *chatSnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{Records: &chatRecords{snapshot: s}, Framing: "composer"}
}
func (s *chatSnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.composer = cursorstore.Composer{}
	delete(s.owner.live, s)
	return nil
}

type chatRecords struct {
	snapshot *chatSnapshot
	next     int
}

func (r *chatRecords) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	s := r.snapshot
	if s.closed || s.owner.closed {
		return agentapi.NativeRecord{}, false, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if r.next == 0 {
		r.next++
		return agentapi.NativeRecord{Kind: "composer", Raw: s.composer.Composer}, true, nil
	}
	i := r.next - 1
	if i >= len(s.composer.Bubbles) {
		return agentapi.NativeRecord{}, false, nil
	}
	b := s.composer.Bubbles[i]
	r.next++
	return agentapi.NativeRecord{Kind: "bubble", Key: b.ID, Raw: b.Value, Missing: b.Missing}, true, nil
}

// Sweep removes only stale, unlocked raw database snapshots.
func (SourceProvider) Sweep() { cursorstore.RemoveStaleSnapshots() }

// Attempts observes the one snapshot preparation attempt allowed in this pass.
func (p *sourcePass) Attempts() int { return p.reader.Attempts() }

// Activities reads database lastUpdatedAt rows in one transaction for all chats.
func (SourceProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	out := map[agentapi.SourceRef]time.Time{}
	var files []agentapi.SourceRef
	var ids []string
	for _, r := range refs {
		if r.Kind == archive.SourceKindFile {
			files = append(files, r)
		} else if r.Kind == archive.SourceKindCursorSQLite && r.Key != "" {
			ids = append(ids, r.Key)
		}
	}
	if len(files) > 0 {
		found, err := (sourceio.FileProvider{}).Activities(ctx, e, files)
		if err != nil {
			return nil, err
		}
		for r, at := range found {
			out[r] = at
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := cursorstore.ReadLastUpdated(ctx, e.Database, ids)
	if err != nil {
		return out, err
	}
	for _, r := range refs {
		if ms := found[r.Key]; r.Kind == archive.SourceKindCursorSQLite && ms > 0 {
			out[r] = time.UnixMilli(ms)
		}
	}
	return out, nil
}
