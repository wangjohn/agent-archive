package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// RefilterRetainedSource owns the existing retained transform and transfers all
// returned output leases. The caller keeps the original input lease alive.
func RefilterRetainedSource(ctx context.Context, reg archive.SessionRegistration, adapter TranscriptFilter, input archive.SourceBundle, budget *NativeReadBudget) (out archive.SourceBundle, release func(), err error) {
	if budget == nil {
		return out, nil, ErrReadBudget
	}
	if err = ctx.Err(); err != nil {
		return out, nil, err
	}
	if retainedRefilterIdentityChanged(input, reg) {
		return out, nil, errors.New("retained refilter identity mismatch")
	}
	if err = input.ValidateHistory(); err != nil {
		return out, nil, err
	}
	const sizingScratch = 32 << 10
	if !budget.Reserve(sizingScratch) {
		return out, nil, ErrReadBudget
	}
	largest := int64(0)
	for _, record := range input.NativeRecords {
		n, e := jsonwire.Bound(ctx, record, budget.Available())
		if e != nil {
			err = e
			break
		}
		largest = max(largest, n)
	}
	for _, text := range input.NativeText {
		largest = max(largest, int64(len(text.Content)))
	}
	budget.Release(sizingScratch)
	if err != nil {
		return out, nil, err
	}
	scanner := int64(64 << 10)
	for scanner < largest+1 && scanner < int64(archive.MaxRecordBytes+1) {
		scanner = min(scanner*2, int64(archive.MaxRecordBytes+1))
	}
	if largest > (budget.Available()-scanner)/3 {
		return out, nil, ErrReadBudget
	}
	scratch := 3*largest + scanner
	if !budget.Reserve(scratch) {
		return out, nil, ErrReadBudget
	}
	defer budget.Release(scratch)
	var leases []func()
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for i := len(leases) - 1; i >= 0; i-- {
				leases[i]()
			}
		})
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	filtered, closeFilter, err := refilterRetainedRows(ctx, reg, adapter, input, budget)
	if err != nil {
		return out, nil, err
	}
	leases = append(leases, closeFilter)
	var decoded int64
	for _, row := range filtered.Records {
		if int64(len(row)) > budget.Available()-decoded {
			return out, nil, ErrReadBudget
		}
		decoded += int64(len(row))
	}
	if !budget.Reserve(decoded) {
		return out, nil, ErrReadBudget
	}
	leases = append(leases, func() { budget.Release(decoded) })
	out, err = archive.NewSourceBundle(reg, adapter, filtered, input.Capture.CapturedAt, input.SupplementalEvidence)
	if err != nil {
		return out, nil, err
	}
	out.Capture.Harness = input.Capture.Harness
	gaps := out.Capture.Gaps
	out.Capture.Gaps = slices.Clone(input.Capture.Gaps)
	for _, gap := range gaps {
		if !slices.Contains(out.Capture.Gaps, gap) {
			out.Capture.Gaps = append(out.Capture.Gaps, gap)
		}
	}
	if err = out.ValidateHistory(); err != nil {
		return out, nil, err
	}
	// Ordinary maps are independently decoded; detach their envelope before ending
	// encoded row ownership. Graph, ordinal and text aliases retain that owner.
	if ordinaryRefilterEnvelope(out, filtered) {
		var closeEnvelope func()
		out, closeEnvelope, err = detachRefilterEnvelope(ctx, out, budget)
		if err != nil {
			return out, nil, err
		}
		leases = append(leases, closeEnvelope)
		leases[0]()
		leases[0] = func() {}
	}
	if err = ctx.Err(); err != nil {
		return out, nil, err
	}
	return out, closeAll, nil
}

func refilterRetainedRows(ctx context.Context, reg archive.SessionRegistration, adapter TranscriptFilter, input archive.SourceBundle, budget *NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	var filtered archive.FilteredTranscript
	var closeFilter func()
	var err error
	if leased, ok := adapter.(LeasedTranscriptRefilter); ok {
		filtered, closeFilter, err = leased.RefilterLeased(ctx, input, reg.SessionStartedAt, budget)
		if err != nil {
			return filtered, nil, err
		}

	} else {
		bounded, ok := adapter.(BoundedTranscriptRefilter)
		if !ok {
			return filtered, nil, ErrReadBudget
		}
		capBytes := min(int64(32<<20), max(int64(0), (budget.Available()-(64<<10))/2))
		if capBytes <= 0 || !budget.Reserve(capBytes) {
			return filtered, nil, ErrReadBudget
		}
		filtered, err = bounded.RefilterBounded(ctx, input, reg.SessionStartedAt, ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: capBytes, ReadBudget: budget})
		if err != nil {
			budget.Release(capBytes)
			return filtered, nil, err
		}
		n := int64(filtered.Boundary.RetainedBytes)
		if n < 0 || n > capBytes {
			budget.Release(capBytes)
			return filtered, nil, ErrReadBudget
		}
		budget.Release(capBytes - n)
		closeFilter = func() { budget.Release(n) }
	}
	return filtered, closeFilter, nil
}

func detachRefilterEnvelope(ctx context.Context, out archive.SourceBundle, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	envelope := out
	envelope.NativeRecords = nil
	envelope.NativeText = nil
	envelope.Ordinals = nil
	if !budget.Reserve((32 << 10)) {
		return out, nil, ErrReadBudget
	}
	n, e := jsonwire.Bound(ctx, envelope, budget.Available()/2)
	budget.Release((32 << 10))
	if e != nil {
		return out, nil, e
	}
	if !budget.Reserve(2 * n) {
		return out, nil, ErrReadBudget
	}
	raw, e := json.Marshal(envelope)
	if e != nil {
		budget.Release(2 * n)
		return out, nil, e
	}
	var detached archive.SourceBundle
	e = json.Unmarshal(raw, &detached)
	budget.Release(n)
	if e != nil {
		budget.Release(n)
		return out, nil, e
	}
	detached.NativeRecords = out.NativeRecords
	out = detached
	return out, func() { budget.Release(n) }, nil
}

func ordinaryRefilterEnvelope(out archive.SourceBundle, filtered archive.FilteredTranscript) bool {
	return out.History == nil && len(out.Ordinals) == 0 && filtered.History == nil && len(filtered.Ordinals) == 0 && len(filtered.Text) == 0
}

func retainedRefilterIdentityChanged(input archive.SourceBundle, reg archive.SessionRegistration) bool {
	return input.SchemaVersion != archive.SourceSchemaVersion && input.SchemaVersion != archive.HistorySourceSchemaVersion || input.ArchiveSessionID != reg.ArchiveSessionID || input.NativeSessionID != reg.NativeSessionID || input.ProjectID != reg.ProjectID || input.Capture.Harness.Name != reg.Harness.Name || input.ParentSessionID != reg.ParentSessionID
}
