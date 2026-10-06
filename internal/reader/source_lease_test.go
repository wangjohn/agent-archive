package reader

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestRetainedLeasePressureReleaseCancellationAndCorruption(t *testing.T) {
	metadata, want, store := fixture(t)
	data, err := store.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(256 << 10)
	pressure := int64(224 << 10)
	if !budget.Reserve(pressure) {
		t.Fatal("reserve")
	}
	_, release, err := DecodeReferencedSourceLeased(t.Context(), metadata, data, Limits{}, budget)
	if !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("pressure refusal %v", err)
	}
	release()
	used, _ := budget.Charged()
	if used != pressure {
		t.Fatalf("refusal leaked %d", used)
	}
	budget.Release(pressure)
	bundle, release, err := DecodeReferencedSourceLeased(t.Context(), metadata, data, Limits{}, budget)
	if err != nil || bundle.NativeSessionID != want.NativeSessionID {
		t.Fatalf("small useful progress %#v %v", bundle, err)
	}
	used, peak := budget.Charged()
	if used <= 0 || peak > 256<<10 {
		t.Fatalf("lease charges %d/%d", used, peak)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(release)
	}
	wg.Wait()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("release leaked %d", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, release, err = DecodeReferencedSourceLeased(ctx, metadata, data, Limits{}, budget)
	release()
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, release, err = DecodeReferencedSourceLeased(t.Context(), metadata, []byte("corrupt"), Limits{}, budget)
	release()
	if err == nil {
		t.Fatal("accepted corruption")
	}
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("failure leaked %d", used)
	}
}
