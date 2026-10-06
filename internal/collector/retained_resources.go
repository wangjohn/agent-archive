package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var errRetainedBudget = agentapi.ReadBudgetLimit(errors.New("retained work exceeds shared data budget"))

func (s *sessionScan) readBudget() *agentapi.NativeReadBudget {
	if s.opts.sourcePasses != nil {
		return s.opts.sourcePasses.env.ReadBudget
	}
	if s.retainedBudget == nil {
		if shared, ok := s.opts.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
			s.retainedBudget = shared.NativeReadBudget()
		}
		if s.retainedBudget == nil {
			s.retainedBudget = agentapi.NewNativeReadBudget(128 << 20)
		}
	}
	return s.retainedBudget
}

// releaseRetained ends the scan's ownership of retained logical data. Scratch
// used only during a bounded operation is released by that operation instead.
func (s *sessionScan) releaseRetained() {
	s.releaseRetainedAfter(0)
}

// releaseRetainedAfter releases only work created after a scope marker. A caller
// must not use any data owned by the scope after returning.
func (s *sessionScan) releaseRetainedAfter(mark int) {
	for i := len(s.retainedReleases) - 1; i >= mark; i-- {
		s.retainedReleases[i]()
	}
	s.retainedReleases = s.retainedReleases[:mark]
}

func (s *sessionScan) retainCharge(n int64) error {
	b := s.readBudget()
	if !b.Reserve(n) {
		return errRetainedBudget
	}
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return nil
}

func (s *sessionScan) historyGet(key string, limit int64) ([]byte, error) {
	b := s.readBudget()
	// Sidecars are mutable: stat provides a bounded allocation ceiling, never
	// authority. Exact bytes/schema/predecessor checks still run after GET.
	if statter, ok := s.remote.(storage.ObjectStatter); ok && strings.HasSuffix(key, "/metadata.json") {
		info, err := statter.Stat(s.ctx, key)
		if err != nil {
			return nil, err
		}
		if info.Size <= 0 || info.Size > limit {
			return nil, errRetainedBudget
		}
		limit = info.Size
	}
	if limit <= 0 || limit > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	if !b.Reserve(limit) {
		return nil, errRetainedBudget
	}
	data, err := historyLimitedGet(s.ctx, s.remote, key, limit)
	if err != nil {
		b.Release(limit)
		return nil, err
	}
	n := int64(len(data))
	b.Release(limit - n)
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return data, nil
}

func (s *sessionScan) historyStage(stage state.PendingSource) ([]byte, error) {
	n := int64(stage.Reference.CompressedBytes)
	if n <= 0 || n > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	b := s.readBudget()
	if !b.Reserve(n) {
		return nil, errRetainedBudget
	}
	data, err := s.local.ReadPendingSource(s.id(), stage)
	if err != nil {
		b.Release(n)
		return nil, err
	}
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return data, nil
}

func (s *sessionScan) decodeReferenced(metadata archive.Metadata, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeReferencedSourceLeased(s.ctx, metadata, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}

func (s *sessionScan) decodeRevision(metadata archive.Metadata, revision string, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeRevisionSourceLeased(s.ctx, metadata, revision, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}

// refilterRetained reserves each overlapping representation separately. The
// input-row encoder buffer, its returned row, the scanner and parsed input row
// coexist with accumulated encoded output, the safe encoder buffer and decoded
// output. Heap object headers and allocator capacity are measured separately.
func (s *sessionScan) refilterRetained(adapter archive.Adapter, bundle archive.SourceBundle) (archive.SourceBundle, error) {
	if s.reg.Harness.Name != archive.HarnessCodex {
		return refilterBundle(s.ctx, s.reg, adapter, bundle)
	}
	b := s.readBudget()
	const preflightScratch = 32 << 10
	if !b.Reserve(preflightScratch) {
		return archive.SourceBundle{}, errRetainedBudget
	}
	largest := int64(0)
	var sizingErr error
	for _, record := range bundle.NativeRecords {
		n, err := jsonwire.Bound(s.ctx, record, b.Available())
		if err != nil {
			sizingErr = err
			break
		}
		largest = max(largest, n)
	}
	b.Release(preflightScratch)
	if sizingErr != nil {
		return archive.SourceBundle{}, errors.Join(errRetainedBudget, sizingErr)
	}
	scanner := int64(64 << 10)
	for scanner < largest+1 && scanner < int64(archive.MaxRecordBytes+1) {
		scanner = min(scanner*2, int64(archive.MaxRecordBytes+1))
	}
	// Three distinct row owners, plus the scanner, are temporary. Check additions
	// against available capacity before forming the sum.
	if largest > (b.Available()-scanner)/3 {
		return archive.SourceBundle{}, errRetainedBudget
	}
	scratch := largest + largest + largest + scanner
	if !b.Reserve(scratch) {
		return archive.SourceBundle{}, errRetainedBudget
	}
	defer b.Release(scratch)
	capBytes := min(int64(32<<20), max(int64(0), (b.Available()-(64<<10))/2))
	if capBytes <= 0 {
		return archive.SourceBundle{}, errRetainedBudget
	}
	// Accumulated records and decoded output each have an independently
	// enforced ceiling. The codec borrows encoder scratch per safe row,
	// leaving headroom rather than reserving a whole second output for it.

	if !b.Reserve(capBytes) {
		return archive.SourceBundle{}, errRetainedBudget
	}
	defer b.Release(capBytes)
	if !b.Reserve(capBytes) {
		return archive.SourceBundle{}, errRetainedBudget
	}
	out, err := refilterBundleBounded(s.ctx, s.reg, adapter, bundle, agentapi.ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: capBytes, ReadBudget: b})
	if err != nil {
		b.Release(capBytes)
		return archive.SourceBundle{}, errors.Join(errRetainedBudget, err)
	}
	n := int64(out.Capture.Boundary.RetainedBytes)
	if n < 0 || n > capBytes {
		b.Release(capBytes)
		return archive.SourceBundle{}, errRetainedBudget
	}
	b.Release(capBytes - n)
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return out, nil
}

// compressSource keeps the canonical codec and charges output before each
// append. Line encoding has two owners: json.Marshal's encoder and result.
func (s *sessionScan) compressSource(bundle archive.SourceBundle) (archive.CompressedSource, error) {
	b := s.readBudget()
	// Gzip/flate uses a 64KiB window, bounded hash/token tables and block writer.
	// One MiB covers their logical fixed scratch for the supported Go toolchain;
	// allocator headers and pooled capacity are measured separately.
	const compressorScratch = 1 << 20
	const preflightScratch = 32 << 10
	if !b.Reserve(preflightScratch) {
		return archive.CompressedSource{}, errRetainedBudget
	}
	largest, err := archive.SourceEncodingLineBound(s.ctx, bundle, b.Available())
	b.Release(preflightScratch)
	if err != nil {
		return archive.CompressedSource{}, errors.Join(errRetainedBudget, err)
	}
	if largest > (b.Available()-compressorScratch)/2 {
		return archive.CompressedSource{}, errRetainedBudget
	}
	scratch := largest + largest + compressorScratch
	if !b.Reserve(scratch) {
		return archive.CompressedSource{}, errRetainedBudget
	}
	defer b.Release(scratch)
	mark := len(s.retainedReleases)
	output := &retainedOutput{scan: s}
	if err := archive.CompressSource(output, bundle); err != nil {
		s.releaseRetainedAfter(mark)
		return archive.CompressedSource{}, err
	}
	data := output.Bytes()
	sum := sha256.Sum256(data)
	return archive.CompressedSource{Bytes: data, SHA256: hex.EncodeToString(sum[:])}, nil
}

type retainedOutput struct {
	bytes.Buffer
	scan *sessionScan
}

func (w *retainedOutput) Write(data []byte) (int, error) {
	if err := w.scan.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > historyCompressedLimit-int64(w.Len()) {
		return 0, errRetainedBudget
	}
	if err := w.scan.retainCharge(int64(len(data))); err != nil {
		return 0, err
	}
	return w.Buffer.Write(data)
}

func (s *sessionScan) marshalRetained(value any) ([]byte, error) {
	b := s.readBudget()
	const scratch = 32 << 10
	if !b.Reserve(scratch) {
		return nil, errRetainedBudget
	}
	n, err := jsonwire.Bound(s.ctx, value, b.Available()/2)
	b.Release(scratch)
	if err != nil {
		return nil, errors.Join(errRetainedBudget, err)
	}
	if !b.Reserve(n) {
		return nil, errRetainedBudget
	}
	defer b.Release(n)
	if !b.Reserve(n) {
		return nil, errRetainedBudget
	}
	data, err := json.Marshal(value)
	if err != nil {
		b.Release(n)
		return nil, err
	}
	actual := int64(len(data))
	if actual > n {
		b.Release(n)
		return nil, errRetainedBudget
	}
	b.Release(n - actual)
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(actual) })
	return data, nil
}

func (s *sessionScan) unmarshalRetained(data []byte, value any) error {
	mark := len(s.retainedReleases)
	if err := s.retainCharge(int64(len(data))); err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		s.releaseRetainedAfter(mark)
		return err
	}
	return nil
}

// newSourceBundle reserves decoded native records before conversion. Text and
// supplemental inputs keep their existing ownership; no duplicate text copy is
// charged merely because the bundle aliases immutable filtered text.
func (s *sessionScan) newSourceBundle(reg archive.SessionRegistration, adapter archive.Adapter, filtered archive.FilteredTranscript, at time.Time, supplemental []archive.SupplementalEvidence) (archive.SourceBundle, error) {
	var n int64
	for _, row := range filtered.Records {
		if int64(len(row)) > s.readBudget().Available()-n {
			return archive.SourceBundle{}, errRetainedBudget
		}
		n += int64(len(row))
	}
	mark := len(s.retainedReleases)
	if err := s.retainCharge(n); err != nil {
		return archive.SourceBundle{}, err
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, supplemental)
	if err != nil {
		s.releaseRetainedAfter(mark)
	}
	return bundle, err
}
