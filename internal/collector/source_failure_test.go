package collector

import (
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

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
