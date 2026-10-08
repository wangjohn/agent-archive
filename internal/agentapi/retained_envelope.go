package agentapi

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"sync"
)

// DetachRetainedEnvelope gives supplemental observations and capture facts an
// independent owner; native records/text/ordinals remain borrowed from input.
func DetachRetainedEnvelope(ctx context.Context, bundle archive.SourceBundle, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	envelope := bundle
	envelope.NativeRecords, envelope.NativeText, envelope.Ordinals = nil, nil, nil
	const scratch = 32 << 10
	if !budget.Reserve(scratch) {
		return archive.SourceBundle{}, nil, ErrReadBudget
	}
	n, err := jsonwire.Bound(ctx, envelope, budget.Available()/3)
	budget.Release(scratch)
	if err != nil {
		return archive.SourceBundle{}, nil, err
	}
	if !budget.Reserve(3 * n) {
		return archive.SourceBundle{}, nil, ErrReadBudget
	}
	raw, err := json.Marshal(envelope)
	var out archive.SourceBundle
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		budget.Release(3 * n)
		return archive.SourceBundle{}, nil, err
	}
	budget.Release(2 * n)
	out.NativeRecords, out.NativeText, out.Ordinals = bundle.NativeRecords, bundle.NativeText, bundle.Ordinals
	var once sync.Once
	return out, func() { once.Do(func() { budget.Release(n) }) }, nil
}
