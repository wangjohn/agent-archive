package collector

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestLocalBundleMissingSourceRetainsCleanupFailure(t *testing.T) {
	fault := errors.New("synthetic source cleanup fault")
	reg := archive.SessionRegistration{ArchiveSessionID: "session", TranscriptPath: "/synthetic/transcript.jsonl", Harness: archive.Harness{Name: "codex"}}
	_, err := ReadLocalBundle(t.Context(), newTestStore(t).Home(), reg, time.Time{}, "", missingCleanupSources{fault: fault})
	if !errors.Is(err, fault) || !agentapi.HasFailure(err, agentapi.Cleanup) || errors.Is(err, ErrNoTranscript) {
		t.Fatalf("missing source hid cleanup or allowed archive fallback: %v", err)
	}
	_, err = ReadLocalBundle(t.Context(), newTestStore(t).Home(), reg, time.Time{}, "", missingCleanupSources{})
	if !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("ordinary missing source no longer allows archive fallback: %v", err)
	}
}

type missingCleanupSources struct{ fault error }

func (s missingCleanupSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	_, f, ok := testSources.LookupSources(name)
	return missingCleanupProvider{fault: s.fault}, f, ok
}

type missingCleanupProvider struct {
	sourceio.FileProvider
	fault error
}

func (p missingCleanupProvider) OpenPass(context.Context, agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return missingCleanupPass{fault: p.fault}, nil
}

type missingCleanupPass struct {
	agentapi.SourcePass
	fault error
}

func (p missingCleanupPass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	return nil, agentapi.Wrap(agentapi.Missing, os.ErrNotExist)
}

func (p missingCleanupPass) Close() error { return agentapi.Wrap(agentapi.Cleanup, p.fault) }

func TestLimitWithTransientVerificationDoesNotSettle(t *testing.T) {
	for _, kind := range []agentapi.FailureKind{agentapi.Changed, agentapi.Unavailable, agentapi.Cleanup} {
		t.Run(string(kind), func(t *testing.T) {
			local := newTestStore(t)
			const id = "transient-limit"
			at := time.Unix(100, 0)
			reg := registration(t, "/synthetic/transcript.jsonl")
			reg.ArchiveSessionID = id
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := local.SaveRequest(id, "stop", at); err != nil {
				t.Fatal(err)
			}
			requests, err := local.LoadRequests()
			if err != nil || len(requests) != 1 {
				t.Fatalf("requests: %v %v", requests, err)
			}
			published, err := local.LoadPublishedState(id)
			if err != nil {
				t.Fatal(err)
			}
			s := sessionScan{local: local, published: published, reg: reg, req: requests[0], opts: Options{Sources: testSources}}
			_, adapter, _ := testSources.LookupSources("codex")
			read := sourceRead{adapter: adapter, observed: observe(archive.SourceKindFile, agentapi.SourceObservation{Present: true, Signature: sourceio.FileSignature(3, 100), Size: 3})}
			failure := errors.Join(errRecordTooLarge, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge), agentapi.Wrap(kind, transcriptio.ErrChanged))
			if _, err := s.readFailed(read, failure); !errors.Is(err, transcriptio.ErrChanged) {
				t.Fatalf("transient failure settled: %v", err)
			}
			if _, blocked, err := local.LoadBlocked(id); err != nil || blocked {
				t.Fatalf("transient read blocked: %v %v", blocked, err)
			}
			if _, found, err := local.LoadScanSignature(id); err != nil || found {
				t.Fatalf("transient read cached: %v %v", found, err)
			}
			if requests, err := local.LoadRequests(); err != nil || len(requests) != 1 {
				t.Fatalf("transient read acknowledged request: %v %v", requests, err)
			}
		})
	}
}
