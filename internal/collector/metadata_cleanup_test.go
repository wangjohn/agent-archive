package collector

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestParserRefreshRetainsNativeProbeCleanupFailure(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		readCleanup  bool
		closeCleanup bool
	}{
		{name: "missing with cleanup", readCleanup: true, closeCleanup: false},
		{name: "descriptor close", readCleanup: false, closeCleanup: true},
		{name: "ordinary missing", readCleanup: false, closeCleanup: false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			checkParserRefreshCleanup(t, scenario.readCleanup, scenario.closeCleanup)
		})
	}
}

func checkParserRefreshCleanup(t *testing.T, readCleanup, closeCleanup bool) {
	t.Helper()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("first publication: %+v %v", result, err)
	}
	prior := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	// Without a settled observation, the refresh must inspect native input
	// before deciding whether normal capture will publish new content.
	if err := local.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic native probe cleanup failure")
	closes := 0
	switch {
	case readCleanup:
		opts.Sources = candidateFaultSources{read: errors.Join(agentapi.Wrap(agentapi.Missing, os.ErrNotExist), agentapi.Wrap(agentapi.Cleanup, fault)), files: nil}
	case closeCleanup:
		opts.Sources = candidateFaultSources{read: nil, files: candidateCloseFiles{fault: fault, closes: &closes}}
	default:
		opts.Sources = missingCleanupSources{}
	}
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	warning := result.Errors[reg.ArchiveSessionID]
	if !readCleanup && !closeCleanup {
		if len(result.Errors) != 0 {
			t.Fatalf("ordinary missing native source prevented best-effort refresh: %+v", result)
		}
	} else if len(result.Errors) != 1 || !errors.Is(warning, fault) || !agentapi.HasFailure(warning, agentapi.Cleanup) {
		t.Fatalf("refresh hid native cleanup: %+v", result)
	}
	if closeCleanup && closes != 1 {
		t.Fatalf("native descriptor closed %d times", closes)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.Parser.Version != "two" || next.SourceBundle != prior.SourceBundle || !next.CapturedAt.Equal(prior.CapturedAt) || len(result.Published) != 1 {
		t.Fatalf("best-effort metadata refresh changed retained source or failed: %+v result=%+v", next, result)
	}
}
