package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const relatedRawBudget int64 = 128 << 20

// Describe retains ordinary append protection; revision publication is separately fenced.
func (SourceProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(ref)
}

// OpenPass owns a bounded shared dependency cache and every descriptor it opens.
func (SourceProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	legacy, err := (sourceio.FileProvider{}).OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	if e.Files == nil {
		e.Files = transcriptio.OS{}
	}
	return &relatedSourcePass{env: e, legacy: legacy, files: map[string]*rolloutFile{}, live: map[*historySnapshot]bool{}}, nil
}

// Activities preserves cheap file ordering independently of history selection.
func (SourceProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	return (sourceio.FileProvider{}).Activities(ctx, e, refs)
}

type rolloutFile struct {
	ref       agentapi.SourceRef
	file      *transcriptio.Snapshot
	meta      codexmeta.CodexMeta
	identity  codexmeta.CodexIdentity
	header    []byte
	boundary  int64
	refs      int
	prefix    []byte
	validated *prefixValidation
}

type relatedSourcePass struct {
	env       agentapi.SourceEnvironment
	legacy    agentapi.SourcePass
	files     map[string]*rolloutFile
	live      map[*historySnapshot]bool
	bytes     int64
	cacheHits int64
	closed    bool
	closeErr  error
}

func sourceFailure(kind agentapi.FailureKind, message string) error {
	return agentapi.Wrap(kind, errors.New(message))
}

func (p *relatedSourcePass) open(ctx context.Context, ref agentapi.SourceRef) (*rolloutFile, error) {
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Path) > 8192 || len(ref.Key) > 4096 {
		return nil, sourceFailure(agentapi.Limit, "rollout locator limit")
	}
	if ref.Kind != archive.SourceKindFile {
		return nil, sourceFailure(agentapi.Unsafe, "invalid rollout kind")
	}
	if old := p.files[ref.Path]; old != nil {
		if err := old.file.Check(); err == nil && old.file.CheckPrefix(ctx, 0, sha256.Sum256(nil)) == nil {
			return old, nil
		}
		if old.refs > 0 {
			return nil, sourceFailure(agentapi.Changed, "rollout dependency changed")
		}
		p.bytes -= old.charge()
		delete(p.files, ref.Path)
		if err := old.file.Close(); err != nil {
			return nil, agentapi.Wrap(agentapi.Cleanup, err)
		}
	}
	if len(p.files) >= archive.MaxHistorySpans {
		return nil, sourceFailure(agentapi.Limit, "shared rollout file limit")
	}
	f, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	fail := func(e error) (*rolloutFile, error) {
		return nil, errors.Join(e, agentapi.Wrap(agentapi.Cleanup, f.Close()))
	}
	if f.Length() > relatedRawBudget {
		stamp := f.Stamp()
		o := agentapi.SourceObservation{Present: true, Size: stamp.Size, Activity: stamp.ModifiedAt, Signature: sourceio.FileSignature(stamp.Size, stamp.ModifiedAt.UnixNano())}
		return fail(&agentapi.SourceError{Kind: agentapi.Limit, Err: agentapi.ErrRawLimit, Observed: &o})
	}

	if p.bytes+f.Length() > relatedRawBudget || len(p.files) >= archive.MaxHistorySpans {
		return fail(sourceFailure(agentapi.Limit, "shared rollout budget exhausted"))
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(f, 0, min(f.Length(), int64(64<<10)+1)), 4096)
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 64<<10 {
		return fail(sourceFailure(agentapi.Unavailable, "rollout metadata incomplete"))
	}
	meta, _, found, err := codexmeta.ParseCodexMeta(line)
	if err != nil || !found {
		return fail(sourceFailure(agentapi.FormatMismatch, "invalid rollout metadata"))
	}
	if meta.HistoryMode != "" && meta.HistoryMode != codexmeta.CodexHistoryLegacy && meta.HistoryMode != codexmeta.CodexHistoryPaginated {
		return fail(sourceFailure(agentapi.Unsafe, "unsupported history mode"))
	}
	identity, outcome := meta.Identity(ref.Path)
	if outcome != "" {
		return fail(sourceFailure(agentapi.FormatMismatch, string(outcome)))
	}
	boundary, err := transcriptio.CompleteJSONLBoundary(f, f.Length(), archive.MaxRecordBytes)
	if err != nil {
		return fail(sourceio.Classify(err))
	}
	out := &rolloutFile{ref: ref, file: f, meta: meta, identity: identity, header: line, boundary: boundary}
	p.files[ref.Path] = out
	p.bytes += f.Length()
	return out, nil
}

func (p *relatedSourcePass) evict() error {
	var err error
	for path, f := range p.files {
		if f.refs == 0 {
			err = errors.Join(err, agentapi.Wrap(agentapi.Cleanup, f.file.Close()))
			p.bytes -= f.charge()
			delete(p.files, path)
		}
	}
	return err
}

func (p *relatedSourcePass) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	err := p.legacy.Close()
	for s := range p.live {
		err = errors.Join(err, s.Close())
	}
	for _, f := range p.files {
		err = errors.Join(err, agentapi.Wrap(agentapi.Cleanup, f.file.Close()))
		f.prefix = nil
		f.validated = nil
	}
	p.files = nil
	p.bytes = 0
	p.closeErr = err
	return err
}

type sourceSelection struct {
	leaf   *rolloutFile
	set    agentapi.CodexRolloutSet
	thread string
}

func (p *relatedSourcePass) selectSource(ctx context.Context, ref agentapi.SourceRef) (sourceSelection, error) {
	if len(p.files) >= 32 || p.bytes > relatedRawBudget/2 {
		if err := p.evict(); err != nil {
			return sourceSelection{}, err
		}
	}
	seed, err := p.open(ctx, ref)
	if err != nil {
		return sourceSelection{}, err
	}
	selected := sourceSelection{leaf: seed, thread: seed.identity.ThreadID}
	if p.env.CodexRollouts == nil {
		if seed.identity.RolloutID != seed.identity.ThreadID || seed.identity.HistoryBase != nil {
			return selected, sourceFailure(agentapi.Unavailable, "current rollout evidence unavailable")
		}
		return selected, nil
	}
	set, err := p.env.CodexRollouts.Thread(ctx, selected.thread)
	if err != nil {
		return selected, err
	}
	if len(set.Revision) > 4096 || len(set.Candidates) > archive.MaxHistorySpans {
		return selected, sourceFailure(agentapi.Limit, "rollout candidate limit")
	}
	for _, candidate := range set.Candidates {
		if len(candidate.Path) > 8192 || len(candidate.Key) > 4096 || candidate.Kind != archive.SourceKindFile {
			return selected, sourceFailure(agentapi.Unsafe, "invalid rollout candidate locator")
		}
	}
	selected.set = set
	if set.Current != nil {
		current, e := p.open(ctx, *set.Current)
		if e != nil {
			return selected, e
		}
		if current.identity.ThreadID != selected.thread {
			return selected, sourceFailure(agentapi.Unsafe, "current rollout identity mismatch")
		}
		selected.leaf = current
		return selected, nil
	}
	return p.selectLineage(ctx, selected)
}

func (p *relatedSourcePass) selectLineage(ctx context.Context, selected sourceSelection) (sourceSelection, error) {
	set := selected.set
	if !set.Complete {
		return selected, sourceFailure(agentapi.Unavailable, "rollout lineage incomplete")
	}
	candidates := map[string]*rolloutFile{}
	bases := map[string]bool{}
	for _, candidate := range set.Candidates {
		f, e := p.open(ctx, candidate)
		if e != nil {
			return selected, e
		}
		id := f.identity
		if id.ThreadID != selected.thread {
			return selected, sourceFailure(agentapi.Unsafe, "rollout candidate identity mismatch")
		}
		if prior := candidates[id.RolloutID]; prior != nil && prior.ref.Path != f.ref.Path {
			return selected, sourceFailure(agentapi.Unavailable, "ambiguous physical rollout")
		}
		candidates[id.RolloutID] = f
		if id.HistoryBase != nil {
			bases[id.HistoryBase.RolloutID] = true
		}
	}
	var tip *rolloutFile
	for id, f := range candidates {
		if !bases[id] {
			if tip != nil {
				return selected, sourceFailure(agentapi.Unavailable, "ambiguous rollout tips")
			}
			tip = f
		}
	}
	if tip == nil {
		return selected, sourceFailure(agentapi.Unavailable, "no complete rollout tip")
	}
	spans, err := p.graph(ctx, tip)
	if err != nil {
		return selected, err
	}
	for _, span := range spans {
		delete(candidates, span.file.identity.RolloutID)
	}
	if len(candidates) != 0 {
		return selected, sourceFailure(agentapi.Unavailable, "disconnected rollout lineage")
	}
	selected.leaf = tip
	return selected, nil
}

type physicalSpan struct {
	file         *rolloutFile
	end          int64
	startOrdinal uint64
	endOrdinal   uint64
	digest       [32]byte
}

func (p *relatedSourcePass) graph(ctx context.Context, leaf *rolloutFile) ([]physicalSpan, error) {
	var reversed []physicalSpan
	seen := map[string]bool{}
	current := leaf
	end, err := newlineBoundary(leaf.file, leaf.file.Length())
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := current.identity
		if seen[id.RolloutID] {
			return nil, sourceFailure(agentapi.Unsafe, "rollout history cycle")
		}
		seen[id.RolloutID] = true
		if len(reversed) >= archive.MaxHistorySpans {
			return nil, sourceFailure(agentapi.Limit, "rollout history depth limit")
		}
		start := uint64(0)
		if id.HistoryBase != nil {
			start = id.HistoryBase.EndOrdinal
		}
		reversed = append(reversed, physicalSpan{file: current, end: end, startOrdinal: start})
		if id.HistoryBase == nil {
			break
		}
		if p.env.CodexRollouts == nil {
			return nil, sourceFailure(agentapi.Unavailable, "history dependency unavailable")
		}
		refs, err := p.env.CodexRollouts.Rollout(ctx, id.HistoryBase.RolloutID)
		if err != nil {
			return nil, err
		}
		if len(refs) != 1 {
			return nil, sourceFailure(agentapi.Unavailable, "history dependency missing or ambiguous")
		}
		base, err := p.open(ctx, refs[0])
		if err != nil {
			return nil, err
		}
		if base.identity.RolloutID != id.HistoryBase.RolloutID {
			return nil, sourceFailure(agentapi.Unsafe, "history dependency identity mismatch")
		}
		if id.HistoryBase.EndByteOffset > math.MaxInt64 {
			return nil, sourceFailure(agentapi.Unsafe, "invalid history byte boundary")
		}
		end = int64(id.HistoryBase.EndByteOffset)
		if end > base.boundary {
			return nil, sourceFailure(agentapi.Unsafe, "invalid history byte boundary")
		}
		var last [1]byte
		if end > 0 {
			if _, err := base.file.ReadAt(last[:], end-1); err != nil || last[0] != '\n' {
				return nil, sourceFailure(agentapi.Unsafe, "split history record boundary")
			}
		}
		current = base
	}
	slices.Reverse(reversed)
	return reversed, nil
}

func hasRelated(f *rolloutFile) bool {
	id := f.identity
	return id.Child || id.ForkID != "" || id.HistoryBase != nil || id.RolloutID != id.ThreadID
}

func (p *relatedSourcePass) Signature(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceObservation, error) {
	if p.env.CodexRollouts == nil && codexmeta.RolloutID(ref.Path) == "" {
		return p.legacy.Signature(ctx, ref)
	}
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		if p.env.CodexRollouts == nil && !agentapi.HasFailure(err, agentapi.Cleanup) && (agentapi.Failure(err) == agentapi.FormatMismatch || agentapi.Failure(err) == agentapi.Unavailable) {
			return p.legacy.Signature(ctx, ref)
		}
		return agentapi.SourceObservation{}, err
	}
	if !hasRelated(selection.leaf) {
		if p.env.CodexRollouts == nil {
			return p.legacy.Signature(ctx, ref)
		}
		return p.observation(ctx, selection, []physicalSpan{{file: selection.leaf, end: selection.leaf.boundary}})
	}
	spans, err := p.graph(ctx, selection.leaf)
	if err != nil {
		return agentapi.SourceObservation{}, err
	}
	return p.observation(ctx, selection, spans)
}

func (p *relatedSourcePass) observation(ctx context.Context, selection sourceSelection, spans []physicalSpan) (agentapi.SourceObservation, error) {
	h := sha256.New()
	_, _ = fmt.Fprint(h, "codex-history-v1\x00", selection.set.Revision, "\x00", selection.leaf.ref.Path)
	candidatePaths := make([]string, 0, len(selection.set.Candidates))
	for _, ref := range selection.set.Candidates {
		candidatePaths = append(candidatePaths, string(ref.Kind)+"\x00"+ref.Path+"\x00"+ref.Key)
	}
	slices.Sort(candidatePaths)
	for _, candidate := range candidatePaths {
		_, _ = fmt.Fprint(h, "\x00candidate:", candidate)
	}
	out := agentapi.SourceObservation{Present: true}
	for _, span := range spans {
		stamp := span.file.file.Stamp()
		out.Size += span.end
		if stamp.ModifiedAt.After(out.Activity) {
			out.Activity = stamp.ModifiedAt
		}
		_, _ = fmt.Fprintf(h, "\x00%s:%d:%d:%d:%d:%x", span.file.ref.Path, stamp.Size, stamp.ModifiedAt.UnixNano(), span.end, span.startOrdinal, sha256.Sum256(span.file.header))
	}
	if p.env.CodexRollouts != nil {
		if err := p.env.CodexRollouts.Check(ctx, selection.thread, selection.set.Revision); err != nil {
			return out, sourceio.Classify(err)
		}
	}
	out.Empty = out.Size == 0
	out.Signature = agentapi.SourceSignature{Version: 1, Provider: "file", Token: hex.EncodeToString(h.Sum(nil))}
	return out, nil
}

func (p *relatedSourcePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.env.CodexRollouts == nil && codexmeta.RolloutID(ref.Path) == "" {
		return p.legacy.Read(ctx, ref, limits)
	}
	fail := func(err error) (agentapi.SourceSnapshot, error) { return nil, errors.Join(err, p.evict()) }
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		if p.env.CodexRollouts == nil && !agentapi.HasFailure(err, agentapi.Cleanup) && (agentapi.Failure(err) == agentapi.FormatMismatch || agentapi.Failure(err) == agentapi.Unavailable) {
			return p.legacy.Read(ctx, ref, limits)
		}
		return fail(err)
	}
	if !hasRelated(selection.leaf) {
		snapshot, err := p.ordinary(ctx, limits, selection)
		if err != nil {
			return fail(err)
		}
		return snapshot, nil
	}
	spans, err := p.graph(ctx, selection.leaf)
	if err != nil {
		return fail(err)
	}
	var size int64
	for _, span := range spans {
		size += span.end
	}
	if size > relatedRawBudget || limits.RawBytes > 0 && size > limits.RawBytes {
		return fail(sourceFailure(agentapi.Limit, "aggregate history byte limit"))
	}
	limit := limits.RecordBytes
	if limit <= 0 || limit > archive.MaxRecordBytes {
		limit = archive.MaxRecordBytes
	}
	var largest int64
	for _, span := range spans {
		largest = max(largest, span.end)
	}
	bufferCharge := min(max(int64(4096), min(largest+1, limit+1)), relatedRawBudget-p.bytes)
	if bufferCharge < 4096 {
		return fail(sourceFailure(agentapi.Limit, "shared record buffer budget exhausted"))
	}
	limit = min(limit, bufferCharge-1)
	s := &historySnapshot{owner: p, selection: selection, spans: spans, recordLimit: limit, bufferCharge: bufferCharge}
	p.bytes += bufferCharge
	for _, span := range spans {
		span.file.refs++
	}
	p.live[s] = true
	if err = p.cachePrefixes(ctx, spans); err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	if err = s.validate(ctx); err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	s.observed, err = p.observation(ctx, selection, spans)
	if err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	return s, nil
}

type historySnapshot struct {
	owner        *relatedSourcePass
	selection    sourceSelection
	spans        []physicalSpan
	history      archive.SourceHistory
	observed     agentapi.SourceObservation
	recordLimit  int64
	bufferCharge int64
	span         int
	scanner      *bufio.Scanner
	nextOrdinal  uint64
	headerSent   bool
	closed       bool
}

func (s *historySnapshot) validate(ctx context.Context) error {
	id := s.selection.leaf.identity
	s.history = archive.SourceHistory{ThreadID: id.ThreadID, ActiveRolloutID: id.RolloutID, OwnStart: id.SubagentOrdinal}
	for _, span := range s.spans {
		facts := span.file.identity
		if facts.ThreadID != id.ThreadID {
			continue
		}
		if facts.ForkID != "" && facts.ForkOrdinal == nil {
			return sourceFailure(agentapi.Unavailable, "fork ownership boundary unavailable")
		}
		for _, boundary := range []*uint64{facts.ForkOrdinal, facts.SubagentOrdinal} {
			if boundary != nil && (s.history.OwnStart == nil || *boundary > *s.history.OwnStart) {
				value := *boundary
				s.history.OwnStart = &value
			}
		}
	}
	count := 0
	for i := range s.spans {
		span := &s.spans[i]
		first := count
		facts, err := s.validateSpan(ctx, span)
		if err != nil {
			return err
		}
		count += facts.records
		if count > archive.MaxHistoryRecords {
			return sourceFailure(agentapi.Limit, "history record limit")
		}
		span.digest, span.endOrdinal = facts.digest, facts.endOrdinal
		next := facts.endOrdinal
		if i+1 < len(s.spans) && next != s.spans[i+1].startOrdinal {
			return sourceFailure(agentapi.Unsafe, "history prefix ordinal mismatch")
		}
		s.history.Spans = append(s.history.Spans, archive.HistorySpan{RolloutID: span.file.identity.RolloutID, ThreadID: span.file.identity.ThreadID, FirstRecord: first, EndRecord: count, StartOrdinal: span.startOrdinal, EndOrdinal: next})
	}
	return s.history.Validate(id.ThreadID, count)
}

func (s *historySnapshot) Observation() agentapi.SourceObservation { return s.observed }

func (s *historySnapshot) Input() agentapi.NativeInput { return agentapi.NativeInput{Records: s} }

func (s *historySnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	delete(s.owner.live, s)
	s.owner.bytes -= s.bufferCharge
	s.bufferCharge = 0
	for _, span := range s.spans {
		span.file.refs--
	}
	s.scanner = nil
	s.spans = nil
	s.selection = sourceSelection{}
	return nil
}

func (s *historySnapshot) ValidateAdmission(ctx context.Context, a agentapi.SourceAdmission) error {
	if s.closed || s.owner.closed {
		return agentapi.ErrClosed
	}
	if a.NativeID != s.selection.thread || a.Cwd != "" && a.Cwd != s.selection.leaf.meta.Cwd {
		return sourceFailure(agentapi.Unsafe, "source admission identity changed")
	}
	return s.check(ctx)
}

func (s *historySnapshot) check(ctx context.Context) error {
	for _, span := range s.spans {
		if err := span.file.file.CheckPrefix(ctx, span.end, span.digest); err != nil {
			return sourceio.Classify(err)
		}
	}
	if s.owner.env.CodexRollouts != nil {
		return sourceio.Classify(s.owner.env.CodexRollouts.Check(ctx, s.selection.thread, s.selection.set.Revision))
	}
	return ctx.Err()
}

func (s *historySnapshot) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if s.closed || s.owner.closed {
		return agentapi.NativeRecord{}, false, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if !s.headerSent {
		s.headerSent = true
		return agentapi.NativeRecord{Kind: agentapi.CodexHistoryHeader, History: &s.history, Raw: s.selection.leaf.header}, true, nil
	}
	for s.span < len(s.spans) {
		span := s.spans[s.span]
		if s.scanner == nil {
			s.scanner = bufio.NewScanner(io.NewSectionReader(span.reader(), 0, span.end))
			s.scanner.Buffer(make([]byte, min(int64(4096), s.recordLimit+1)), int(s.recordLimit)+1)
			s.nextOrdinal = span.startOrdinal
		}
		if s.scanner.Scan() {
			raw := s.scanner.Bytes()
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			ordinal := s.nextOrdinal
			s.nextOrdinal++
			return agentapi.NativeRecord{Kind: agentapi.CodexHistoryRecord, Key: span.file.identity.RolloutID, Raw: raw, Ordinal: ordinal}, true, nil
		}
		if err := s.scanner.Err(); err != nil {
			if errors.Is(err, bufio.ErrTooLong) {
				return agentapi.NativeRecord{}, false, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
			}
			return agentapi.NativeRecord{}, false, sourceio.Classify(err)
		}
		s.scanner = nil
		s.span++
	}
	return agentapi.NativeRecord{}, false, s.check(ctx)
}

// ordinarySnapshot retains file framing while accepting verified later appends.
type ordinarySnapshot struct {
	owner     *relatedSourcePass
	source    *rolloutFile
	selection sourceSelection
	ctx       context.Context
	length    int64
	digest    [32]byte
	closed    bool
	observed  agentapi.SourceObservation
}

func (p *relatedSourcePass) ordinary(ctx context.Context, limits agentapi.ReadLimits, selection sourceSelection) (agentapi.SourceSnapshot, error) {
	f := selection.leaf
	if limits.RawBytes > 0 && f.file.Length() > limits.RawBytes {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f.file.Reader(ctx), f.boundary)); err != nil {
		return nil, err
	}
	stamp := f.file.Stamp()
	out := &ordinarySnapshot{owner: p, source: f, selection: selection, ctx: ctx, length: f.boundary, observed: agentapi.SourceObservation{Signature: sourceio.FileSignature(stamp.Size, stamp.ModifiedAt.UnixNano()), Present: true, Empty: f.boundary == 0, Activity: stamp.ModifiedAt, Size: stamp.Size}}
	copy(out.digest[:], h.Sum(nil))
	if p.env.CodexRollouts != nil {
		var err error
		out.observed, err = p.observation(ctx, selection, []physicalSpan{{file: f, end: f.boundary}})
		if err != nil {
			return nil, err
		}
	}
	f.refs++
	return out, nil
}

func (s *ordinarySnapshot) Observation() agentapi.SourceObservation { return s.observed }

func (s *ordinarySnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{File: ordinaryFile{FileInput: s.source.file, s: s}}
}

func (s *ordinarySnapshot) Close() error {
	if !s.closed {
		s.closed = true
		s.source.refs--
	}
	return nil
}

type ordinaryFile struct {
	agentapi.FileInput
	s *ordinarySnapshot
}

func (f ordinaryFile) Length() int64 { return f.s.length }

func (f ordinaryFile) ReadAt(b []byte, off int64) (int, error) {
	if f.s.closed || f.s.owner.closed {
		return 0, agentapi.ErrClosed
	}
	return f.FileInput.ReadAt(b, off)
}

func (f ordinaryFile) Check() error {
	if f.s.closed || f.s.owner.closed {
		return agentapi.ErrClosed
	}
	return f.s.check(f.s.ctx)
}

// newlineBoundary excludes even valid JSON until its producer commits the newline.
func newlineBoundary(file io.ReaderAt, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	buf := make([]byte, 64<<10)
	floor := max(int64(0), size-archive.MaxRecordBytes-1)
	for end := size; end > floor; {
		start := max(floor, end-int64(len(buf)))
		part := buf[:end-start]
		if _, err := file.ReadAt(part, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(part, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
}

func (f ordinaryFile) Records(ctx context.Context, tail bool, windowBytes, recordBytes int64, visit func([]byte) bool) (transcriptio.RecordWindow, error) {
	if f.s.closed || f.s.owner.closed {
		return transcriptio.RecordWindow{}, agentapi.ErrClosed
	}
	return f.FileInput.Records(ctx, tail, windowBytes, recordBytes, visit)
}

func (s *ordinarySnapshot) ValidateAdmission(ctx context.Context, a agentapi.SourceAdmission) error {
	if s.closed || s.owner.closed {
		return agentapi.ErrClosed
	}
	if a.NativeID != s.source.identity.ThreadID || a.Cwd != "" && a.Cwd != s.source.meta.Cwd {
		return sourceFailure(agentapi.Unsafe, "source admission identity changed")
	}
	return s.check(ctx)
}

func (s *ordinarySnapshot) check(ctx context.Context) error {
	if err := s.source.file.CheckPrefix(ctx, s.length, s.digest); err != nil {
		return sourceio.Classify(err)
	}
	if s.owner.env.CodexRollouts != nil {
		return sourceio.Classify(s.owner.env.CodexRollouts.Check(ctx, s.selection.thread, s.selection.set.Revision))
	}
	return ctx.Err()
}

// prefixValidation is cached only for one immutable, fully captured ancestor prefix.
type prefixValidation struct {
	startOrdinal   uint64
	endOrdinal     uint64
	records        int
	maxRecordBytes int64
	digest         [32]byte
}

const prefixSummaryCharge int64 = 128

func (f *rolloutFile) charge() int64 {
	charge := f.file.Length()
	if f.prefix != nil {
		charge += int64(len(f.prefix)) + prefixSummaryCharge
	}
	return charge
}

func (span physicalSpan) reader() io.ReaderAt {
	if int64(len(span.file.prefix)) >= span.end {
		return bytes.NewReader(span.file.prefix[:span.end])
	}
	return span.file.file
}

func (p *relatedSourcePass) cachePrefixes(ctx context.Context, spans []physicalSpan) error {
	for _, span := range spans[:len(spans)-1] {
		f := span.file
		if f.prefix != nil || span.end > relatedRawBudget/4 || p.bytes+span.end+prefixSummaryCharge > relatedRawBudget {
			continue
		}
		// The one bounded immutable copy is shared by descendants; never replace it
		// while a snapshot might hold a borrowed scanner over its bytes.
		raw := make([]byte, span.end)
		reader := sourceio.Reader(ctx, f.file, span.end)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return sourceio.Classify(err)
		}
		f.prefix = raw
		p.bytes += span.end + prefixSummaryCharge
	}
	return nil
}

func (s *historySnapshot) validateSpan(ctx context.Context, span *physicalSpan) (prefixValidation, error) {
	cached := span.file.validated
	if cached != nil && int64(len(span.file.prefix)) == span.end && cached.startOrdinal == span.startOrdinal {
		if cached.maxRecordBytes > s.recordLimit {
			return prefixValidation{}, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		s.owner.cacheHits++
		return *cached, ctx.Err()
	}
	facts := prefixValidation{startOrdinal: span.startOrdinal, endOrdinal: span.startOrdinal}
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(io.NewSectionReader(span.reader(), 0, span.end), h))
	scanner.Buffer(make([]byte, min(int64(4096), s.recordLimit+1)), int(s.recordLimit)+1)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return facts, err
		}
		facts.maxRecordBytes = max(facts.maxRecordBytes, int64(len(scanner.Bytes())))
		if facts.maxRecordBytes > s.recordLimit {
			return facts, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record struct {
			Ordinal *uint64 `json:"ordinal"`
		}
		if json.Unmarshal(line, &record) != nil {
			return facts, sourceFailure(agentapi.FormatMismatch, "invalid history record")
		}
		if span.file.identity.HistoryMode == codexmeta.CodexHistoryPaginated && record.Ordinal == nil {
			return facts, sourceFailure(agentapi.Unsafe, "missing history ordinal")
		}
		if record.Ordinal != nil && *record.Ordinal != facts.endOrdinal {
			return facts, sourceFailure(agentapi.Unsafe, "noncontiguous history ordinal")
		}
		if facts.endOrdinal == math.MaxUint64 {
			return facts, sourceFailure(agentapi.Limit, "history ordinal overflow")
		}
		facts.endOrdinal++
		facts.records++
		if facts.records > archive.MaxHistoryRecords {
			return facts, sourceFailure(agentapi.Limit, "history record limit")
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return facts, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		return facts, sourceio.Classify(err)
	}
	copy(facts.digest[:], h.Sum(nil))
	if int64(len(span.file.prefix)) == span.end {
		span.file.validated = &facts
	}
	return facts, nil
}
