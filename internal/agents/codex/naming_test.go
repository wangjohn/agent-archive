package codex

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestNamingOnlyChangePreservesRealEvidenceAcrossCodecUpgrade(t *testing.T) {
	_, request := labelFixture(t)
	old := request.Bundle
	old.Capture.FilterVersion = "16"
	next := old
	next.Capture.AdapterVersion = "0.17.0"
	next.Capture.FilterVersion = "17"
	next.Capture.CapturedAt = old.Capture.CapturedAt.Add(time.Hour)
	if (Filter{}).NamingOnlyChange(old, next) {
		t.Fatal("codec change without a changed name claimed naming-only proof")
	}
	label := archive.SessionLabel{NativeID: labelTestID, State: "present", Name: "Renamed", Source: "database", Contract: archive.SessionLabelContract}
	next.SupplementalEvidence = []archive.SupplementalEvidence{label.Evidence(next.Capture.CapturedAt)}
	if !(Filter{}).NamingOnlyChange(old, next) {
		t.Fatal("bookkeeping upgrade and owning label was treated as activity")
	}
	for _, mode := range []string{"legacy", "paginated"} {
		upgraded := next
		upgraded.NativeRecords = []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2", "history_mode": mode}}, next.NativeRecords[1]}
		if !(Filter{}).NamingOnlyChange(old, upgraded) {
			t.Fatal("newly retained representation enum renewed capture")
		}
		explicit := old
		explicit.NativeRecords = []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2", "history_mode": map[string]string{"legacy": "paginated", "paginated": "legacy"}[mode]}}, old.NativeRecords[1]}
		if (Filter{}).NamingOnlyChange(explicit, upgraded) {
			t.Fatal("explicit native representation change was ignored")
		}
	}
	for _, mutate := range []func(*archive.SourceBundle){
		func(b *archive.SourceBundle) { b.ProjectID = "other" },
		func(b *archive.SourceBundle) { b.Capture.Harness.Version = "0.160.0" },
		func(b *archive.SourceBundle) {
			b.NativeText = []archive.TextTranscript{{Format: "text", Content: "New activity"}}
		},
		func(b *archive.SourceBundle) { b.ParentSessionID = "parent" },
		func(b *archive.SourceBundle) {
			b.SupplementalEvidence = append(b.SupplementalEvidence, archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, Payload: map[string]any{"text": "New response"}})
		},
		func(b *archive.SourceBundle) {
			b.NativeRecords = append(append([]map[string]any(nil), b.NativeRecords...), map[string]any{"type": "event_msg"})
		},
	} {
		changed := next
		mutate(&changed)
		if (Filter{}).NamingOnlyChange(old, changed) {
			t.Fatal("real evidence change was treated as naming only")
		}
	}
}
