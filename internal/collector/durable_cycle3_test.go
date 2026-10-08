package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type cycle3WorkKind string

const (
	cycle3Healthy          cycle3WorkKind = "healthy"
	cycle3Corrupt          cycle3WorkKind = "corrupt"
	cycle3Opaque           cycle3WorkKind = "opaque"
	cycle3SourceOnly       cycle3WorkKind = "source-only"
	cycle3Future           cycle3WorkKind = "future"
	cycle3ExtraReceipt     cycle3WorkKind = "extra-receipt"
	cycle3UnclearedReceipt cycle3WorkKind = "uncleared-receipt"
)

func cycle3Owed(t *testing.T, local *state.Store, id, agent, native string, kind cycle3WorkKind) (string, []byte) {
	t.Helper()
	if kind == cycle3Healthy {
		return "", nil
	}
	if err := config.WithDurableStorage(local.Home(), func(config.DurableStorageGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "pending", id+".json")
	raw := []byte("{original")
	switch kind {
	case cycle3Healthy:
	case cycle3Corrupt:
	case cycle3Future:
		raw = []byte(`{"history":{"version":2}}`)
	case cycle3Opaque:
		path = filepath.Join(local.Home(), "publication-evidence", id, "opaque")
	case cycle3SourceOnly:
		sum := sha256.Sum256(raw)
		stage, err := local.StagePendingSource(id, archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(raw)}, raw)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(local.SessionDir(id), "pending-sources", stage.Name), raw
	case cycle3ExtraReceipt, cycle3UnclearedReceipt:
		path = filepath.Join(local.Home(), "generation-recovery", id+".json")
		suffix := `,"future":null}`
		if kind == cycle3UnclearedReceipt {
			suffix = `,"request":{}}`
		}
		raw = []byte(`{"version":1,"key":{"Agent":"` + agent + `","NativeID":"` + native + `"},"previous":"` + id + `","next":"next","complete":true` + suffix)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

type cycle3Sources struct {
	reads      int
	signatures int
	filters    int
	ledger     *agentapi.NativeReadBudget
}

func (s *cycle3Sources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	return cycle3Provider{SourceProvider: p, count: s}, cycle3Filter{TranscriptFilter: f, count: s}, ok
}

type cycle3Provider struct {
	agentapi.SourceProvider
	count *cycle3Sources
}

func (p cycle3Provider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.count.ledger = e.ReadBudget
	pass, err := p.SourceProvider.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return cycle3SourcePass{SourcePass: pass, count: p.count}, nil
}

type cycle3SourcePass struct {
	agentapi.SourcePass
	count *cycle3Sources
}

func (p cycle3SourcePass) Signature(ctx context.Context, r agentapi.SourceRef) (agentapi.SourceObservation, error) {
	p.count.signatures++
	return p.SourcePass.Signature(ctx, r)
}

func (p cycle3SourcePass) Read(ctx context.Context, r agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	p.count.reads++
	return p.SourcePass.Read(ctx, r, l)
}

type cycle3Filter struct {
	agentapi.TranscriptFilter
	count *cycle3Sources
}

func (f cycle3Filter) Filter(ctx context.Context, input agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.count.filters++
	return f.TranscriptFilter.Filter(ctx, input, c)
}

func TestCandidateChildRecoveryRefusesBeforeSourceAndAdmission(t *testing.T) {
	for _, kind := range []cycle3WorkKind{cycle3Healthy, cycle3Corrupt, cycle3Opaque, cycle3SourceOnly, cycle3Future, cycle3ExtraReceipt, cycle3UnclearedReceipt} {
		t.Run(string(kind), func(t *testing.T) {
			local := newTestStore(t)
			start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "child.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
			if err := local.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "child-native", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: parent.Harness, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
			if err := local.SaveSubagentCandidate(candidate); err != nil {
				t.Fatal(err)
			}
			candidatePath := filepath.Join(local.Home(), "subagent-candidates", "child.json")
			candidateBytes, err := os.ReadFile(candidatePath)
			if err != nil {
				t.Fatal(err)
			}
			owedPath, owed := cycle3Owed(t, local, "child", "claude", "child-native", kind)
			sources := &cycle3Sources{}
			opts := Options{Sources: sources}
			closeSources := openCursorPass(nil, &opts)
			defer func() {
				if err := closeSources(); err != nil {
					t.Error(err)
				}
			}()
			before := opts.sourcePasses.env.ReadBudget.Available()
			outcome := materializeSubagentCandidates(t.Context(), local, opts, candidate.ObservedAt)
			if kind == cycle3Healthy {
				if len(outcome.errors) != 0 || sources.filters != 1 || sources.ledger != opts.sourcePasses.env.ReadBudget {
					t.Fatalf("healthy candidate %+v filters=%d", outcome, sources.filters)
				}
				return
			}
			if !errors.Is(outcome.errors["child"], state.ErrDurableStorageRecovery) || sources.reads != 0 || sources.filters != 0 {
				t.Errorf("child recovery reached source: %+v reads=%d filter=%d", outcome, sources.reads, sources.filters)
			}
			if _, found, err := local.LoadRegistration("child"); err != nil || found {
				t.Errorf("child registered: %t %v", found, err)
			}
			if raw, err := os.ReadFile(candidatePath); err != nil || !bytes.Equal(raw, candidateBytes) {
				t.Errorf("candidate changed: %v", err)
			}
			if raw, err := os.ReadFile(owedPath); err != nil || !bytes.Equal(raw, owed) {
				t.Errorf("child original changed: %v", err)
			}
			if opts.sourcePasses.env.ReadBudget.Available() != before {
				t.Error("child eligibility retained shared budget")
			}
		})
	}
}

func TestSettledCursorRecoveryRefusesBeforeSignature(t *testing.T) {
	for _, kind := range []cycle3WorkKind{cycle3Healthy, cycle3Corrupt, cycle3Opaque, cycle3SourceOnly, cycle3ExtraReceipt, cycle3UnclearedReceipt} {
		t.Run(string(kind), func(t *testing.T) {
			local := newTestStore(t)
			db := newCursorDB(t, true)
			db.chatSaying("chat", 1000, "first draft", "b1", "b2")
			reg := cursorRegistration("session", "chat")
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			sources := &cycle3Sources{}
			remote := storagetest.NewMemoryStore()
			opts := Options{Sources: sources, Parsers: testParsers, MachineID: "m", CursorDatabase: db.path, Now: advancingClock()}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Published) != 1 {
				t.Fatal(result, err)
			}
			if _, found, err := local.LoadScanSignature(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("missing settled signature", err)
			}
			owedPath, owed := cycle3Owed(t, local, reg.ArchiveSessionID, "cursor", "chat", kind)
			sources.reads, sources.signatures, sources.filters = 0, 0, 0
			result, err := Run(t.Context(), local, remote, opts)
			if kind == cycle3Healthy {
				if err != nil || len(result.Errors) != 0 || !contains(result.Skipped, reg.ArchiveSessionID) || sources.filters != 0 || sources.signatures == 0 {
					t.Fatalf("healthy unchanged cursor %+v %v signatures=%d", result, err, sources.signatures)
				}
				return
			}
			if !errors.Is(err, state.ErrDurableStorageRecovery) && !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) {
				t.Errorf("recovery absent: %+v %v", result, err)
			}
			if sources.signatures+sources.reads+sources.filters != 0 {
				t.Errorf("recovery reached Cursor: signature=%d reads=%d filter=%d", sources.signatures, sources.reads, sources.filters)
			}
			if raw, err := os.ReadFile(owedPath); err != nil || !bytes.Equal(raw, owed) {
				t.Errorf("owed bytes changed: %v", err)
			}
		})
	}
}

func TestRegisteredUnsupportedReceiptRefusesNamingAndPreview(t *testing.T) {
	for _, kind := range []cycle3WorkKind{cycle3ExtraReceipt, cycle3UnclearedReceipt} {
		t.Run(string(kind), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "native.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			provider := &mutableLabels{label: archive.SessionLabel{State: archive.SessionLabelPresent, Name: "retained", Source: archive.SessionLabelDatabase, Contract: "synthetic"}}
			lookup := &recoveryLabelLookup{provider: provider}
			remote := storagetest.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }, Labels: lookup}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
				t.Fatal(result, err)
			}
			path, original := cycle3Owed(t, local, reg.ArchiveSessionID, "codex", reg.NativeSessionID, kind)
			now = now.Add(time.Hour)
			provider.calls, lookup.acquisitions = 0, 0
			result, err := Run(t.Context(), local, remote, opts)
			if !errors.Is(err, state.ErrDurableStorageRecovery) && !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrDurableStorageRecovery) {
				t.Errorf("receipt ignored: %+v %v", result, err)
			}
			status, statusErr := local.LoadStatus()
			if statusErr != nil || status.PendingCount != 1 {
				t.Errorf("named receipt not counted once: %+v %v", status, statusErr)
			}
			if provider.calls+lookup.acquisitions != 0 {
				t.Errorf("receipt reached naming: provider=%d acquisitions=%d", provider.calls, lookup.acquisitions)
			}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			_, previewErr := ReadLocalBundle(t.Context(), local.Home(), reg, now, "", bindings)
			if !errors.Is(previewErr, state.ErrDurableStorageRecovery) || filter.calls != 0 {
				t.Errorf("receipt reached preview: filter=%d err=%v", filter.calls, previewErr)
			}
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, original) {
				t.Error("receipt changed", err)
			}
		})
	}
}

func TestUnsupportedReceiptHoldsOnlyItsRegisteredSession(t *testing.T) {
	local := newTestStore(t)
	bad := registration(t, writeTranscript(t, t.TempDir(), "bad.jsonl", codexTranscript))
	good := registration(t, writeTranscript(t, t.TempDir(), "good.jsonl", codexTranscript))
	good.ArchiveSessionID, good.NativeSessionID = "healthy-session", "healthy-native"
	for _, reg := range []archive.SessionRegistration{bad, good} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	path, original := cycle3Owed(t, local, bad.ArchiveSessionID, "codex", bad.NativeSessionID, cycle3ExtraReceipt)
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return good.RegisteredAt.Add(time.Hour) }})
	if err != nil || !errors.Is(result.Errors[bad.ArchiveSessionID], state.ErrDurableStorageRecovery) || !contains(result.Published, good.ArchiveSessionID) {
		t.Fatal(result, err)
	}
	status, err := local.LoadStatus()
	if err != nil || status.PendingCount != 1 {
		t.Fatal(status, err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, original) {
		t.Fatal("receipt changed", err)
	}
}

func TestGenerationRecoveryCancellationPrecedesOwedWork(t *testing.T) {
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		refusal := errors.Join(state.ErrDurableStorageRecovery, cancellation)
		if !generationRecoveryStopsPass(refusal) {
			t.Fatal("joined cancellation admitted pass", refusal)
		}
	}
	if generationRecoveryStopsPass(state.ErrDurableStorageRecovery) {
		t.Fatal("named owed work aborted healthy sessions")
	}
}

func TestRuntimeChildSourceBorrowsExistingPassLedger(t *testing.T) {
	local := newTestStore(t)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "child.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: parent.Harness, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	sources := &cycle3Sources{}
	opts := Options{Sources: sources, Parsers: testParsers, MachineID: "machine", CodexRollouts: recoveryBudgetLookup{budget: budget}, Now: func() time.Time { return start.Add(time.Hour) }}
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), opts)
	if err != nil || sources.ledger != budget || budget.Available() != 128<<20 {
		t.Fatalf("runtime source ledger changed/leaked: %+v %v same=%t available=%d", result, err, sources.ledger == budget, budget.Available())
	}
	if _, found, err := local.LoadRegistration("child"); err != nil || !found {
		t.Fatal("healthy child not admitted", found, err)
	}
}
