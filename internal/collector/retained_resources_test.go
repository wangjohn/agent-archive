package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRetainedRefilterMakesSmallProgressUnderSharedPressure(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	bundle, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	budget := agentapi.NewNativeReadBudget(2 << 20)
	pressure := int64(1 << 20)
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	scan.retainedBudget = budget
	out, err := scan.refilterRetained(codex.Filter{}, bundle)
	if err != nil {
		t.Fatalf("small valid work starved by ceiling: %v", err)
	}
	if err := out.ValidateHistory(); err != nil {
		t.Fatal(err)
	}
	if len(out.NativeRecords) != len(bundle.NativeRecords) {
		t.Fatal("partial refilter output")
	}
	used, peak := budget.Charged()
	if used <= pressure || peak >= 2<<20 {
		t.Fatalf("owned charge %d peak%d", used, peak)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != pressure {
		t.Fatalf("release imbalance %d", used-pressure)
	}
	// Complete exhaustion refuses without retaining output, and does not prevent
	// independent small progress when the unrelated reservation is returned.
	fill := budget.Available()
	if !budget.Reserve(fill) {
		t.Fatal("fill")
	}
	_, err = scan.refilterRetained(codex.Filter{}, bundle)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("refusal %v", err)
	}
	budget.Release(fill)
	budget.Release(pressure)
	if _, err := scan.refilterRetained(codex.Filter{}, bundle); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("independent progress leaked%d", used)
	}
}

func TestRetainedComparisonReservationReleasesOnRefusal(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(32 << 10)
	scan.retainedBudget = budget
	_, err := scan.jsonEncodingsEqual(archive.SupplementalEvidence{Payload: map[string]any{"text": "synthetic"}}, nil)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("comparison refusal leaked%d", used)
	}
}

// HEAD checksum absence is supported by production S3 stores, including older
// objects and multipart checksums. This lane must still use charged bounded GET.
type noChecksumHistoryStore struct {
	*storagetest.MemoryStore
	sourceGets        int
	sourceLimitedGets int
}

func (s *noChecksumHistoryStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	info.SHA256 = ""
	return info, err
}

func (s *noChecksumHistoryStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sourceGets++
		return nil, errors.New("unbounded source verification read")
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *noChecksumHistoryStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sourceLimitedGets++
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestHistoryPublicationWithoutHeadChecksumUsesBoundedVerification(t *testing.T) {
	scan, pending := privacyJournal(t)
	remote := &noChecksumHistoryStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	result := runHistoryRetry(t, scan, remote)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("bounded checksum fallback did not publish: %#v", result)
	}
	if remote.sourceGets != 0 || remote.sourceLimitedGets == 0 {
		t.Fatalf("source verification reads: unbounded=%d bounded=%d", remote.sourceGets, remote.sourceLimitedGets)
	}
	assertCompleteHistory(t, scan, pending, remote.MemoryStore)
}

func TestHistoryVerificationRefusesSharedPressureThenMakesProgress(t *testing.T) {
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	remote := &noChecksumHistoryStore{MemoryStore: cloud}
	scan.remote = remote
	budget := scan.readBudget()
	pressure := budget.Available()
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if err := scan.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("verification bypassed exhausted shared ledger: %v", err)
	}
	if remote.sourceLimitedGets != 0 || remote.sourceGets != 0 {
		t.Fatal("source was read before reserving")
	}
	budget.Release(pressure)
	if err := scan.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("verification lease leaked: %d", used)
	}
}

func TestRevisionSizingRefusesSharedPressureAndReleasesScratch(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(64 << 10)
	pressure := int64(40 << 10)
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	p := revisionPlanner{ctx: t.Context(), budget: budget, plan: &revisionPlan{}}
	b := revisionBundle(t, revisionB, "synthetic retained evidence")
	if _, err := p.checkStageEvidence(b); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("sizing bypassed shared pressure: %v", err)
	}
	if used, _ := budget.Charged(); used != pressure {
		t.Fatalf("refusal changed prior charge: %d", used)
	}
	budget.Release(pressure)
	if n, err := p.checkStageEvidence(b); err != nil || n == 0 {
		t.Fatalf("independent sizing failed: %d %v", n, err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("sizing scratch leaked: %d", used)
	}
}

func TestMetadataRefreshEncodingRefusesSharedPressure(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	bundle, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), bundle, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	var prior archive.Metadata
	if err := json.Unmarshal(rendered.metadata, &prior); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	scan.retainedBudget = agentapi.NewNativeReadBudget(1)
	scan.opts.ParserVersion = "synthetic-parser-upgrade"
	scan.parserResolved = false
	if raw, changed, err := scan.refreshedMetadata(lastPublication{bundle: bundle, metadata: prior}, rendered.source); !errors.Is(err, agentapi.ErrReadBudget) || changed || len(raw) != 0 {
		t.Fatalf("refresh encoded without reservation: changed=%t bytes=%d err=%v", changed, len(raw), err)
	}
	scan.releaseRetained()
	if used, _ := scan.retainedBudget.Charged(); used != 0 {
		t.Fatalf("refresh refusal leaked: %d", used)
	}
}
