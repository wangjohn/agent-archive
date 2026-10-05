package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func settledGenerationFixture(t *testing.T) (*state.Store, *storagetest.MemoryStore, archive.SessionRegistration, Options, time.Time) {
	t.Helper()
	s, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, s, cloud, at)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at.Add(time.Hour) }, RepoKey: func(string) string { return "" }}
	if result, err := Run(t.Context(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("block: %#v %v", result, err)
	}
	reg, _, err := s.LoadRegistration("session-1")
	if err != nil {
		t.Fatal(err)
	}
	build, err := PrepareGenerationRecovery(t.Context(), reg, at.Add(2*time.Hour), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at.Add(2*time.Hour), build); err != nil {
		t.Fatal(err)
	}
	opts.Now = func() time.Time { return at.Add(3 * time.Hour) }
	// The successor first retries its fixed publication, then settles its live
	// source signature. Both generations must cost nothing after that.
	for range 2 {
		if result, err := Run(t.Context(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
			t.Fatalf("settle: %#v %v", result, err)
		}
	}
	reg, _, err = s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s, cloud, reg, opts, at
}

func TestFrozenUnsupportedNodeBlocksParserMaintenance(t *testing.T) {
	s, cloud, reg, opts, _ := settledGenerationFixture(t)
	nodePath := filepath.Join(s.Home(), "generation-nodes", reg.ArchiveSessionID+".json")
	raw, err := os.ReadFile(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	node["version"] = 99
	raw, err = json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	opts.AcceptSession = func(candidate archive.SessionRegistration) bool {
		return candidate.ArchiveSessionID == reg.ArchiveSessionID
	}
	opts.ParserVersion = "unsupported-node-parser-upgrade"
	result, cost := measurePass(t, s, cloud, opts)
	if result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("unsupported node allowed retained maintenance: %#v %+v", result, cost)
	}
}

type frozenWork string

const frozenWorkRequest frozenWork = "request"

const frozenWorkUpload frozenWork = "upload"

const frozenWorkRateLimited frozenWork = "rate-limited"

const frozenWorkScanOnly frozenWork = "scan-only"

func TestFrozenSignatureWaitsForOutstandingWork(t *testing.T) {
	for _, kind := range []frozenWork{frozenWorkRequest, frozenWorkUpload, frozenWorkRateLimited, frozenWorkScanOnly} {
		t.Run(string(kind), func(t *testing.T) {
			s, cloud, reg, opts, at := settledGenerationFixture(t)
			if err := s.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
			published, err := s.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case frozenWorkRequest:
				err = s.SaveRequest(reg.ArchiveSessionID, "feedback", at)
			case frozenWorkUpload:
				err = s.SavePending(reg.ArchiveSessionID, state.PendingPublication{ReadyAt: at, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: "sha", SourceBytes: []byte("source"), MetadataBytes: []byte(`{}`)})
			case frozenWorkRateLimited:
				bundle, _, _ := published.LastPublished()
				err = published.Save(bundle, at, state.CacheStatusRateLimited)
			case frozenWorkScanOnly:
				err = s.SetScanPending(reg.ArchiveSessionID, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), s, cloud, reg, state.Request{}, published, at, opts)
			before := state.PublishedStateLoads()
			if err := scan.recordFrozenSignature(); err != nil {
				t.Fatal(err)
			}
			if state.PublishedStateLoads() != before {
				t.Fatal("frozen signature check decoded retained source")
			}
			_, found, err := s.LoadScanSignature(reg.ArchiveSessionID)
			if err != nil || found != (kind == frozenWorkScanOnly) {
				t.Fatalf("frozen signature with %s work: %v %v", kind, found, err)
			}
		})
	}
}

// Whole-source decode counts are process-wide, so these cost tests are serial.
func TestFrozenGenerationsCostNothingPerPass(t *testing.T) {
	s, cloud, reg, opts, _ := settledGenerationFixture(t)
	for pass := range 2 {
		result, cost := measurePass(t, s, cloud, opts)
		if len(result.Errors) != 0 || len(result.Skipped) != 2 || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
			t.Fatalf("unchanged generation pass %d: %d full decodes, %d writes; %#v", pass, cost.loads, cost.writes, result)
		}
	}
	signature, found, err := s.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found || !signature.Frozen {
		t.Fatalf("no frozen maintenance signature: %#v %v", signature, err)
	}
	// A pre-recovery native signature cannot stand for retained maintenance.
	signature.Frozen = false
	if err := s.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
		t.Fatal(err)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || cost.loads != 1 || cost.writes == 0 {
		t.Fatalf("ordinary token skipped frozen maintenance: %#v %+v", result, cost)
	}
	// Nor may a frozen token suppress ordinary native capture.
	active := reg
	active.CaptureFrozen = false
	p := pass{local: s, opts: opts, ctx: t.Context()}
	if unchanged, _, err := p.unchangedSinceLastScan(active); err != nil || unchanged {
		t.Fatalf("frozen token trusted by native capture: %v %v", unchanged, err)
	}
}

func TestFrozenSignatureInvalidationsMaintainRetainedHistory(t *testing.T) {
	s, cloud, reg, opts, at := settledGenerationFixture(t)
	opts.AcceptSession = func(candidate archive.SessionRegistration) bool {
		return candidate.ArchiveSessionID == reg.ArchiveSessionID
	}
	// Neither steady-state checks nor invalidated maintenance may observe live
	// input. It now contains an invalid native transcript.
	if err := os.WriteFile(reg.TranscriptPath, []byte("not native JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("live rewrite caused frozen work: %#v %+v", result, cost)
	}
	editRetainedRecords(t, s, plantSecret)
	simulateFilterUpgrade(t, s)
	opts.SkillEvidence = config.SkillEvidenceNone
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 1 || cost.loads != 1 {
		t.Fatalf("privacy upgrade skipped retained maintenance: %#v %+v", result, cost)
	}
	assertRefilteredSnapshot(t, cloud, at)
	opts.ParserVersion = "frozen-cost-parser-upgrade"
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 1 || cost.loads != 1 {
		t.Fatalf("parser upgrade skipped retained maintenance: %#v %+v", result, cost)
	}
	if metadata := fetchMetadata(t, cloud, "codex", reg.ArchiveSessionID); !metadata.CapturedAt.Equal(at) || metadata.Parser.Version != opts.ParserVersion {
		t.Fatalf("parser refresh changed capture age: %#v", metadata)
	}
	feedback, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindExplicitFeedback, ObservedAt: at.Add(4 * time.Hour),
		Provenance: "user:agent-archive-feedback-file",
		Payload:    map[string]any{"event_id": "frozen-cost-feedback", "text": "retained feedback password=synthetic-secret", "source": "user"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(reg.ArchiveSessionID, "feedback", at.Add(4*time.Hour), feedback...); err != nil {
		t.Fatal(err)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 1 || cost.loads != 1 {
		t.Fatalf("feedback skipped retained maintenance: %#v %+v", result, cost)
	}
	assertRefilteredSnapshot(t, cloud, at)
	metadata := fetchMetadata(t, cloud, "codex", reg.ArchiveSessionID)
	bundle := fetchBundle(t, cloud, metadata)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(reg.ArchiveSessionID, state.PendingPublication{
		SkillEvidence: string(opts.skillEvidence()), MetadataOnly: true, Bundle: bundle,
		SourceKey: metadata.SourceBundle.Key, SourceSHA256: metadata.SourceBundle.SHA256,
		SourceSize: metadata.SourceBundle.CompressedBytes, MetadataKey: key, MetadataBytes: encoded,
		ReadyAt: at.Add(4 * time.Hour), Attempted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || len(result.Published) != 0 || cost.loads != 1 {
		// Retrying a pending transaction may be reported as skipped after its
		// successful upload; it must still run and settle the pending record.
		t.Fatalf("pending publication skipped: %#v %+v", result, cost)
	}
	if pending, err := s.HasPending(reg.ArchiveSessionID); err != nil || pending {
		t.Fatalf("pending publication not settled: %v %v", pending, err)
	}
	if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
		t.Fatal(err)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || cost.loads != 1 {
		t.Fatalf("interrupted scan skipped: %#v %+v", result, cost)
	}
	if result, cost := measurePass(t, s, cloud, opts); len(result.Errors) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("retained maintenance did not settle: %#v %+v", result, cost)
	}
}
