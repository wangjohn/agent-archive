package collector

import (
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// withCursorRewriteGap keeps one rewrite gap: it replaces the earlier one,
// counting on from it, and leaves other evidence alone.
func TestWithCursorRewriteGapKeepsOne(t *testing.T) {
	at := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	other := archive.SupplementalEvidence{Kind: archive.EvidenceKindCaptureGap, ObservedAt: at, Provenance: "hook:subagent-link", Payload: map[string]any{"code": "x"}}
	evidence := []archive.SupplementalEvidence{other}
	for i := 1; i <= 3; i++ {
		evidence = withCursorRewriteGap(evidence, at.Add(time.Duration(i)*time.Minute))
	}
	if len(evidence) != 2 || evidence[0].Provenance != other.Provenance {
		t.Fatalf("%+v", evidence)
	}
	gap := evidence[1]
	if !gap.ObservedAt.Equal(at.Add(3*time.Minute)) || gap.Payload["detail"] != fmt.Sprintf(cursorRewriteDetail, 3) || gap.Payload["code"] != CaptureGapCursorChatRewritten {
		t.Fatalf("%+v", gap)
	}
	if _, gaps, err := archive.FilterSupplementalEvidence(evidence); err != nil || len(gaps) != 0 {
		t.Fatalf("the gap does not pass the evidence filter unchanged: %v %v", gaps, err)
	}
}
