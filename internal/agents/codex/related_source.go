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
	"os"
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
	if errors.Is(err, os.ErrNotExist) && p.env.CodexRollouts != nil && codexmeta.RolloutID(ref.Key+".jsonl") == ref.Key && ref.Key != "" {
		set, lookupErr := p.env.CodexRollouts.Thread(ctx, ref.Key)
		if lookupErr != nil {
			return sourceSelection{}, lookupErr
		}
		if set.Current != nil {
			seed, err = p.open(ctx, *set.Current)
		} else if set.Complete && len(set.Candidates) > 0 && len(set.Candidates) <= archive.MaxHistorySpans {
			seed, err = p.open(ctx, set.Candidates[0])
		}
	}
	if err != nil {
		return sourceSelection{}, err
	}
	if ref.Key != "" && seed.identity.ThreadID != ref.Key {
		return sourceSelection{}, sourceFailure(agentapi.Unsafe, "registered native thread identity mismatch")
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
	return p.readSelection(ctx, ref, limits, selection)
}

func (p *relatedSourcePass) readSelection(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits, selection sourceSelection) (agentapi.SourceSnapshot, error) {
	fail := func(err error) (agentapi.SourceSnapshot, error) { return nil, errors.Join(err, p.evict()) }
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
	admission    *archive.CodexSourceBinding
	firstOwnTask *agentapi.OwnTaskFacts
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
		if s.firstOwnTask == nil && span.file.identity.ThreadID == s.selection.thread && facts.firstTask.Seen {
			task := facts.firstTask
			s.firstOwnTask = &task
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
	s.admission = a.Binding
	if s.history.OwnStart == nil && a.Binding != nil {
		s.history.OwnStart = a.Binding.OwnStart
	}
	facts, err := s.AdmissionFacts(ctx)
	if err != nil {
		return err
	}
	if !validAdmissionFacts(s.selection.leaf, facts, a) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
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
	facts, err := s.AdmissionFacts(ctx)
	if err != nil {
		return err
	}
	if a.Binding != nil {
		facts.FirstNativeTaskAt = a.Binding.FirstNativeTaskAt
		facts.FirstNativeTaskID = a.Binding.FirstNativeTaskID
	}
	if !validAdmissionFacts(s.source, facts, a) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
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

func bindingFacts(f *rolloutFile, home string, own *uint64) (archive.CodexSourceBinding, error) {
	_, created, found, err := codexmeta.ParseCodexMeta(f.header)
	if err != nil || !found || created.IsZero() {
		return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unavailable, "native creation evidence unavailable")
	}
	var source string
	if json.Unmarshal(f.meta.Source, &source) != nil && len(f.meta.Source) > 0 {
		var value any
		if err := json.Unmarshal(f.meta.Source, &value); err != nil {
			return archive.CodexSourceBinding{}, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
		source = string(encoded)
	}
	root := f.identity.RootID
	if root == "" && !f.identity.Child {
		root = f.identity.ThreadID
	}
	facts := archive.CodexSourceBinding{Version: 1, Child: f.identity.Child, NativeThreadID: f.identity.ThreadID, NativeCreatedAt: created, Cwd: f.meta.Cwd, SelectedCwd: f.meta.Cwd, ProducerSource: source, PhysicalProducerVersion: f.meta.Version, PhysicalProducerOriginator: f.meta.Originator, RootID: root, ParentID: f.identity.ParentID, OwnStart: own, PhysicalRolloutID: f.identity.RolloutID, Path: f.ref.Path, Home: home}
	return facts, facts.Validate()
}

func (s *ordinarySnapshot) AdmissionFacts(ctx context.Context) (archive.CodexSourceBinding, error) {
	if s.closed || s.owner.closed {
		return archive.CodexSourceBinding{}, agentapi.ErrClosed
	}
	if err := s.source.file.CheckPrefix(ctx, s.length, s.digest); err != nil {
		return archive.CodexSourceBinding{}, sourceio.Classify(err)
	}
	return bindingFacts(s.source, s.owner.env.Policy.Root, nil)
}

func (s *historySnapshot) AdmissionFacts(ctx context.Context) (archive.CodexSourceBinding, error) {
	if s.closed || s.owner.closed {
		return archive.CodexSourceBinding{}, agentapi.ErrClosed
	}
	if err := s.check(ctx); err != nil {
		return archive.CodexSourceBinding{}, err
	}
	return s.owner.historyBindingFacts(ctx, s.selection, s.spans, s.history.OwnStart, s.admission)

}

func (p *relatedSourcePass) ValidateSourceAdmission(ctx context.Context, ref agentapi.SourceRef, admission agentapi.SourceAdmission) error {
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		return err
	}
	spans, err := p.graph(ctx, selection.leaf)
	if err != nil {
		return err
	}
	var own *uint64
	for _, span := range spans {
		if span.file.identity.ThreadID != selection.thread {
			continue
		}
		for _, boundary := range []*uint64{span.file.identity.ForkOrdinal, span.file.identity.SubagentOrdinal} {
			if boundary != nil && (own == nil || *boundary > *own) {
				v := *boundary
				own = &v
			}
		}
		digest := sha256.Sum256(span.file.header)
		if err := span.file.file.CheckPrefix(ctx, int64(len(span.file.header)), digest); err != nil {
			return sourceio.Classify(err)
		}
	}
	facts, err := p.historyBindingFacts(ctx, selection, spans, own, admission.Binding)
	if err != nil {
		return err
	}
	if selection.thread != admission.NativeID || admission.Cwd != "" && facts.Cwd != admission.Cwd || !validAdmissionFacts(selection.leaf, facts, admission) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	if p.env.CodexRollouts != nil {
		return p.env.CodexRollouts.Check(ctx, selection.thread, selection.set.Revision)
	}
	return ctx.Err()
}

func validAdmissionFacts(f *rolloutFile, facts archive.CodexSourceBinding, a agentapi.SourceAdmission) bool {
	if a.Binding != nil && a.Binding.PhysicalRolloutID == f.identity.RolloutID {
		if a.Binding.PhysicalProducerVersion != "" && a.Binding.PhysicalProducerVersion != f.meta.Version || a.Binding.PhysicalProducerOriginator != "" && a.Binding.PhysicalProducerOriginator != f.meta.Originator {
			return false
		}
	}
	return facts.PreservesFacts(a.Binding) && (a.NativeCreatedAt.IsZero() || facts.NativeCreatedAt.Equal(a.NativeCreatedAt)) && (a.InitialProducerVersion == "" || f.meta.Version == a.InitialProducerVersion) && (a.InitialProducerOriginator == "" || f.meta.Originator == a.InitialProducerOriginator) && (a.InitialProducerSource == "" || facts.ProducerSource == a.InitialProducerSource)
}

// prefixValidation is cached only for one immutable, fully captured ancestor prefix.
type prefixValidation struct {
	taskOwned      bool
	taskBoundary   uint64
	firstTask      agentapi.OwnTaskFacts
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
		result := *cached
		boundary := uint64(0)
		if s.history.OwnStart != nil {
			boundary = *s.history.OwnStart
		}
		if span.file.identity.ThreadID != s.selection.thread || !cached.taskOwned || cached.taskBoundary != boundary {
			result.firstTask = agentapi.OwnTaskFacts{}
		}
		return result, ctx.Err()
	}
	facts := prefixValidation{startOrdinal: span.startOrdinal, endOrdinal: span.startOrdinal}
	facts.taskOwned = span.file.identity.ThreadID == s.selection.thread
	if s.history.OwnStart != nil {
		facts.taskBoundary = *s.history.OwnStart
	}

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
		if facts.taskOwned && !facts.firstTask.Seen && facts.endOrdinal >= facts.taskBoundary {
			facts.firstTask = ownTaskFacts(line, localExecutionShape(span.file))
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

func (p *relatedSourcePass) ReadRevision(ctx context.Context, ref agentapi.SourceRef, admission agentapi.SourceAdmission, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	leaf, err := p.open(ctx, ref)
	if err != nil {
		return nil, err
	}
	if admission.NativeID == "" || leaf.identity.ThreadID != admission.NativeID {
		return nil, sourceFailure(agentapi.Unsafe, "historical revision belongs to another thread")
	}
	selection := sourceSelection{leaf: leaf, thread: admission.NativeID}
	if p.env.CodexRollouts != nil {
		selection.set, err = p.env.CodexRollouts.Thread(ctx, admission.NativeID)
		if err != nil {
			return nil, err
		}
	}
	snapshot, err := p.readSelection(ctx, ref, limits, selection)
	if err != nil {
		return nil, err
	}
	validator, ok := snapshot.(agentapi.SourceAdmissionValidator)
	if !ok {
		return nil, errors.Join(sourceFailure(agentapi.Unsafe, "historical admission validator unavailable"), snapshot.Close())
	}
	if err := validator.ValidateAdmission(ctx, admission); err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	return snapshot, nil
}

func (s *historySnapshot) RevisionCandidates(ctx context.Context) ([]agentapi.SourceRef, error) {
	if s.closed || s.owner.closed {
		return nil, agentapi.ErrClosed
	}
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	out := make([]agentapi.SourceRef, 0, len(s.spans)+len(s.selection.set.Candidates))
	seen := map[string]bool{}
	add := func(ref agentapi.SourceRef) error {
		if seen[ref.Path] {
			return nil
		}
		if len(out) >= archive.MaxHistorySpans {
			return sourceFailure(agentapi.Limit, "historical candidate limit")
		}
		seen[ref.Path] = true
		out = append(out, ref)
		return nil
	}
	for _, span := range s.spans {
		if span.file.identity.ThreadID == s.selection.thread {
			if err := add(span.file.ref); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range s.selection.set.Candidates {
		if err := add(ref); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ordinarySnapshot) RevisionCandidates(ctx context.Context) ([]agentapi.SourceRef, error) {
	if _, err := s.AdmissionFacts(ctx); err != nil {
		return nil, err
	}
	out := []agentapi.SourceRef{s.source.ref}
	seen := map[string]bool{s.source.ref.Path: true}
	for _, ref := range s.selection.set.Candidates {
		if seen[ref.Path] {
			continue
		}
		if len(out) >= archive.MaxHistorySpans {
			return nil, sourceFailure(agentapi.Limit, "historical candidate limit")
		}
		seen[ref.Path] = true
		out = append(out, ref)
	}
	return out, nil
}

func (p *relatedSourcePass) historyBindingFacts(ctx context.Context, selection sourceSelection, spans []physicalSpan, own *uint64, known *archive.CodexSourceBinding) (archive.CodexSourceBinding, error) {
	var original *rolloutFile
	for _, span := range spans {
		if span.file.identity.ThreadID == selection.thread && span.file.identity.RolloutID == selection.thread {
			original = span.file
			break
		}
	}
	if original == nil && known == nil && p.env.CodexRollouts != nil {
		refs, err := p.env.CodexRollouts.Rollout(ctx, selection.thread)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
		if len(refs) > archive.MaxHistorySpans {
			return archive.CodexSourceBinding{}, sourceFailure(agentapi.Limit, "original fact candidate limit")
		}
		for _, ref := range refs {
			candidate, err := p.open(ctx, ref)
			if err != nil {
				return archive.CodexSourceBinding{}, err
			}
			if candidate.identity.ThreadID == selection.thread && candidate.identity.RolloutID == selection.thread {
				if original != nil && original.ref.Path != candidate.ref.Path {
					return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unavailable, "original native identity ambiguous")
				}
				original = candidate
			}
		}
	}
	var facts archive.CodexSourceBinding
	if original != nil {
		var err error
		facts, err = bindingFacts(original, p.env.Policy.Root, own)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
	} else if known != nil {
		facts = *known
		if own != nil {
			facts.OwnStart = own
		}
	} else {
		return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unavailable, "original native creation facts unavailable")
	}
	for _, span := range spans {
		id := span.file.identity
		if id.ThreadID != selection.thread {
			continue
		}
		if id.RootID != "" {
			if facts.RootID != "" && facts.RootID != id.RootID {
				return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native root identity conflict")
			}
			facts.RootID = id.RootID
		}
		if id.ParentID != "" {
			if facts.ParentID != "" && facts.ParentID != id.ParentID {
				return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native parent identity conflict")
			}
			facts.ParentID = id.ParentID
		}
		if id.Child && !facts.Child {
			return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native child identity conflict")
		}
	}
	if known != nil {
		facts.FirstNativeTaskAt = known.FirstNativeTaskAt
		facts.FirstNativeTaskID = known.FirstNativeTaskID
	}
	facts.PhysicalRolloutID = selection.leaf.identity.RolloutID
	facts.Path = selection.leaf.ref.Path
	facts.SelectedCwd = selection.leaf.meta.Cwd
	facts.PhysicalProducerVersion = selection.leaf.meta.Version
	facts.PhysicalProducerOriginator = selection.leaf.meta.Originator
	return facts, facts.Validate()
}

func ownTaskFacts(line []byte, local bool) agentapi.OwnTaskFacts {
	seen, native := codexmeta.NativeFirstTask(line)
	if !seen {
		return agentapi.OwnTaskFacts{}
	}
	var event struct {
		Payload struct {
			TurnID string `json:"turn_id"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(line, &event)
	return agentapi.OwnTaskFacts{Seen: true, Native: native, LocalExecution: local, StartedAt: codexmeta.FirstTaskAt(line), TurnID: event.Payload.TurnID}
}

func localExecutionShape(f *rolloutFile) bool {
	return f.meta.LocalExecutionSource()
}

func (s *historySnapshot) FirstOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	facts, err := s.gatherOwnTask(ctx)
	if err != nil {
		return facts, err
	}
	return facts, s.check(ctx)
}

func (s *historySnapshot) gatherOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	if s.closed || s.owner.closed {
		return agentapi.OwnTaskFacts{}, agentapi.ErrClosed
	}
	if s.firstOwnTask != nil {
		return *s.firstOwnTask, ctx.Err()
	}
	for _, span := range s.spans {
		if span.file.identity.ThreadID != s.selection.thread {
			continue
		}
		ordinal := span.startOrdinal
		scanner := bufio.NewScanner(io.NewSectionReader(span.file.file, 0, span.end))
		scanner.Buffer(make([]byte, min(4096, int(s.recordLimit)+1)), int(s.recordLimit)+1)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return agentapi.OwnTaskFacts{}, err
			}
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			current := ordinal
			ordinal++
			if s.history.OwnStart != nil && current < *s.history.OwnStart {
				continue
			}
			facts := ownTaskFacts(line, localExecutionShape(span.file))
			if facts.Seen {
				s.firstOwnTask = &facts
				return facts, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return agentapi.OwnTaskFacts{}, sourceio.Classify(err)
		}
	}
	return agentapi.OwnTaskFacts{}, ctx.Err()
}

func (s *ordinarySnapshot) FirstOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	if s.closed || s.owner.closed {
		return agentapi.OwnTaskFacts{}, agentapi.ErrClosed
	}
	scanner := bufio.NewScanner(io.NewSectionReader(s.source.file, 0, s.length))
	scanner.Buffer(make([]byte, 4096), archive.MaxRecordBytes+1)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return agentapi.OwnTaskFacts{}, err
		}
		facts := ownTaskFacts(bytes.TrimSpace(scanner.Bytes()), localExecutionShape(s.source))
		if facts.Seen {
			if err := (ordinaryFile{FileInput: s.source.file, s: s}).Check(); err != nil {
				return agentapi.OwnTaskFacts{}, err
			}
			return facts, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return agentapi.OwnTaskFacts{}, sourceio.Classify(err)
	}
	return agentapi.OwnTaskFacts{}, (ordinaryFile{FileInput: s.source.file, s: s}).Check()
}

func (s *historySnapshot) AdmissionEvidence(ctx context.Context, admission agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	if s.closed || s.owner.closed {
		return agentapi.AdmissionEvidence{}, agentapi.ErrClosed
	}
	s.admission = admission.Binding
	if s.history.OwnStart == nil && admission.Binding != nil && admission.Binding.OwnStart != nil {
		s.history.OwnStart = admission.Binding.OwnStart
		s.firstOwnTask = nil
	}
	binding, err := s.owner.historyBindingFacts(ctx, s.selection, s.spans, s.history.OwnStart, s.admission)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	if admission.NativeID != s.selection.thread || admission.Cwd != "" && admission.Cwd != s.selection.leaf.meta.Cwd || !validAdmissionFacts(s.selection.leaf, binding, admission) {
		return agentapi.AdmissionEvidence{}, sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	task, err := s.gatherOwnTask(ctx)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	evidence := agentapi.AdmissionEvidence{Binding: binding, Task: task}
	return evidence, s.check(ctx)
}

func (s *ordinarySnapshot) AdmissionEvidence(ctx context.Context, admission agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	if s.closed || s.owner.closed {
		return agentapi.AdmissionEvidence{}, agentapi.ErrClosed
	}
	binding, err := bindingFacts(s.source, s.owner.env.Policy.Root, nil)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	if admission.Binding != nil {
		binding.FirstNativeTaskAt = admission.Binding.FirstNativeTaskAt
		binding.FirstNativeTaskID = admission.Binding.FirstNativeTaskID
	}
	if admission.NativeID != s.source.identity.ThreadID || admission.Cwd != "" && admission.Cwd != s.source.meta.Cwd || !validAdmissionFacts(s.source, binding, admission) {
		return agentapi.AdmissionEvidence{}, sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	task, err := s.FirstOwnTask(ctx)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	return agentapi.AdmissionEvidence{Binding: binding, Task: task}, nil
}
