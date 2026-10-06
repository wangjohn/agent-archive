package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"testing"
)

func TestNativeHeaderUnderstandsRelatedIdentitiesWithoutAdmittingCapture(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-0000-0000-000000000003"
	const root = "00000000-0000-0000-0000-000000000001"
	const parent = "00000000-0000-0000-0000-000000000002"
	for _, tc := range []struct {
		fields  string
		rollout string
		want    codexmeta.Outcome
	}{
		{``, id, ""},
		{`,"session_id":"` + root + `","parent_thread_id":"` + parent + `"`, id, codexmeta.ChildHistoryPending},
		{`,"forked_from_id":"` + root + `"`, id, codexmeta.ForkHistoryPending},
		{``, parent, codexmeta.RelatedHistoryPending},
	} {
		data := []byte(`{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic/project"` + tc.fields + `}}`)
		for _, purpose := range []agentapi.DiscoveryPurpose{agentapi.DiscoveryImport, agentapi.DiscoveryHandoff} {
			h, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: purpose, Path: "rollout-" + tc.rollout + ".jsonl", Scan: func(visit func([]byte) bool) error { visit(data); return nil }})
			if err != nil || h.IdentityMismatch || h.NativeID != id || h.CapturePending != expectedHeaderPending(tc.want, purpose) {
				t.Fatalf("header=%+v err=%v", h, err)
			}
			if purpose == agentapi.DiscoveryHandoff && tc.want == codexmeta.ChildHistoryPending && !h.SubagentOnly {
				t.Fatal("incomplete history selectable for handoff")
			}
		}
	}
}

func expectedHeaderPending(want codexmeta.Outcome, purpose agentapi.DiscoveryPurpose) string {
	if (want == codexmeta.ChildHistoryPending || want == codexmeta.ForkHistoryPending) && purpose == agentapi.DiscoveryImport {
		return ""
	}
	return string(want)
}
