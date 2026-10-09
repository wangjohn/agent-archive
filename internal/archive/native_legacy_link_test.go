package archive

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLegacyNativeMarkerPreservesRawEvidenceThroughRoundtripAndRefilter(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ghost, _ := NewLinkedSessionEvidence("legacy-ghost", LinkedSessionUnavailable, at)
	legitimate, _ := NewLinkedSessionEvidence("legitimate-unavailable", LinkedSessionUnavailable, at)
	verified, _ := NewLinkedSessionEvidence("native-child", LinkedSessionPublished, at)
	marker, _ := NewLinkedSessionEvidence("legacy-ghost", LinkedSessionUnavailable, at.Add(time.Minute))
	marker.Provenance = NativeLegacyUnverifiedLinkProvenance
	raw := []SupplementalEvidence{ghost, legitimate, verified, marker}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var restarted []SupplementalEvidence
	if err := json.Unmarshal(encoded, &restarted); err != nil {
		t.Fatal(err)
	}
	filtered, _, err := FilterSupplementalEvidence(restarted)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := FilterSupplementalEvidence(filtered)
	if err != nil {
		t.Fatal(err)
	}
	links := deriveLinkedSessions(again)
	if len(links) != 2 || links[0].SessionID != "legitimate-unavailable" || links[1].SessionID != "native-child" {
		t.Fatalf("ghost/real unavailable/source child folded incorrectly: %+v", links)
	}
	if len(again) != 4 {
		t.Fatal("historical raw observations were removed")
	}
	// With no positive marker, an unavailable link must remain explicitly unknown.
	if links := deriveLinkedSessions(raw[:3]); len(links) != 3 {
		t.Fatal("unknown historical observation blanket dropped")
	}
}

func TestProducerFactsProjectionPreservesKnownImmutableBinding(t *testing.T) {
	const parent = "00000000-0000-0000-0000-000000000001"
	previous := &CodexSourceBinding{NativeThreadID: "00000000-0000-0000-0000-000000000002", RootID: parent, ProducerSource: `{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":1,"agent_nickname":"private additive nickname"}}}`}
	current := *previous
	current.ProducerSource = `{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":1}}}`
	if !current.PreservesFacts(previous) {
		t.Fatal("bounded semantic producer projection changed immutable facts")
	}
	current.ProducerSource = `{"subagent":{"thread_spawn":{"parent_thread_id":"00000000-0000-0000-0000-000000000003","depth":1}}}`
	if current.PreservesFacts(previous) {
		t.Fatal("producer projection permitted a changed native parent")
	}
}
