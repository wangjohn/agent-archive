package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// A reverted physical segment can lack history_base; the original source name
// must reach the native gate through the real shared source/filter pipeline.
func TestCodexPhysicalRelatedRolloutCannotBeCaptured(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-0000-0000-000000000001"
	const other = "00000000-0000-0000-0000-000000000002"
	raw := `{"type":"session_meta","payload":{"id":"` + id + `","session_id":"` + id + `","cwd":"/synthetic/project"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"synthetic inherited message"}}` + "\n"
	for _, rollout := range []string{id, other} {
		path := writeTranscript(t, t.TempDir(), "rollout-"+rollout+".jsonl", raw)
		filtered, _, err := FilterSource(context.Background(), "codex", agentapi.SourceRef{Path: path}, time.Time{}, testSources)
		if rollout == id {
			if err != nil || len(filtered.Records) == 0 {
				t.Fatalf("ordinary capture: %v", err)
			}
		} else if !errors.Is(err, archive.ErrRelatedHistory) || len(filtered.Records) != 0 {
			t.Fatalf("physical related history admitted: %v", err)
		}
	}
}
