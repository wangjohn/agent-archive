package collector

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"reflect"
	"testing"
	"time"
)

func nativeLinkRegistration(id, native, parent, home string, at time.Time) archive.SessionRegistration {
	return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: native, Harness: archive.Harness{Name: "codex"}, ProjectID: "project", ProjectRoot: "/synthetic/project", TranscriptPath: home + "/rollout-" + native + ".jsonl", SessionStartedAt: at, RegisteredAt: at, NativeChild: parent != "", ParentNativeSessionID: parent, NativeSourceHome: home, DestinationID: "destination"}
}

func TestNativeLinksRetryChildrenBeforeParentAndSettleWithoutDecodeOrWrites(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	const parentID = "00000000-0000-0000-0000-000000000001"
	const childID = "00000000-0000-0000-0000-000000000002"
	child := nativeLinkRegistration("child", childID, parentID, home, at)
	if err := local.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	p := &pass{local: local, registrations: []archive.SessionRegistration{child}, now: at}
	if err := p.reconcileNativeLinks(); err != nil || p.registrations[0].ParentSessionID != "" {
		t.Fatalf("unresolved parent blocked or fabricated: %v", err)
	}
	parent := nativeLinkRegistration("parent", parentID, "", home, at)
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	p.registrations = append(p.registrations, parent)
	if err := p.reconcileNativeLinks(); err != nil || p.registrations[0].ParentSessionID != "parent" {
		t.Fatalf("late parent not reconciled: %v", err)
	}
	reg, found, err := local.LoadRegistration("child")
	if err != nil || !found || reg.NativeSessionID != childID || reg.ParentSessionID != "parent" || !reg.HookObservedAt.IsZero() || !reg.ImportBatch.IsZero() || !reg.SessionStartedAt.Equal(child.SessionStartedAt) {
		t.Fatalf("link changed ownership: %+v %v", reg, err)
	}
	before := snapshotMtimes(t, local.Home())
	loads := state.PublishedStateLoads()
	for range 3 {
		if err := p.reconcileNativeLinks(); err != nil {
			t.Fatal(err)
		}
	}
	if state.PublishedStateLoads() != loads || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatal("settled relationship repair decoded published state or wrote session state")
	}
}

func TestNativeLinksRespectHomeProjectDestinationExclusionAndRemoval(t *testing.T) {
	for _, mode := range []string{"home", "project", "destination", "excluded", "removed"} {
		t.Run(mode, func(t *testing.T) {
			local := newTestStore(t)
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			home := t.TempDir()
			const parentID = "00000000-0000-0000-0000-000000000001"
			parent := nativeLinkRegistration("parent", parentID, "", home, at)
			child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parentID, home, at)
			switch mode {
			case "home":
				child.NativeSourceHome = t.TempDir()
			case "project":
				child.ProjectID = "other"
				child.ProjectRoot = "/synthetic/other"
			case "destination":
				child.DestinationID = "other"
			}
			for _, reg := range []archive.SessionRegistration{parent, child} {
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "removed" {
				if err := local.RecordRemoval("codex", parentID, state.RemovalReasonUndo, at); err != nil {
					t.Fatal(err)
				}
			}
			p := &pass{local: local, registrations: []archive.SessionRegistration{child, parent}, now: at, opts: Options{AcceptSession: func(reg archive.SessionRegistration) bool {
				return mode != "excluded" || reg.ArchiveSessionID != "parent"
			}}}
			if err := p.reconcileNativeLinks(); err != nil || p.registrations[0].ParentSessionID != "" {
				t.Fatalf("scope/tombstone bypass mode %s: %v", mode, err)
			}
		})
	}
}

func TestLegacyCodexCompositeGhostKeepsRawEvidenceWithoutDoubleCounting(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	parent := nativeLinkRegistration("parent", "00000000-0000-0000-0000-000000000001", "", home, at)
	child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parent.NativeSessionID, home, at)
	for _, reg := range []archive.SessionRegistration{parent, child} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	oldID, _, err := local.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: parent.NativeSessionID + ":subagent:" + child.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	ghost, err := archive.NewLinkedSessionEvidence(oldID, archive.LinkedSessionUnavailable, at)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := archive.NewLinkedSessionEvidence("legitimate-unavailable", archive.LinkedSessionUnavailable, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(parent.ArchiveSessionID, "legacy-stop", at, ghost, unavailable); err != nil {
		t.Fatal(err)
	}
	p := &pass{local: local, registrations: []archive.SessionRegistration{child, parent}, now: at.Add(time.Minute)}
	if err := p.reconcileNativeLinks(); err != nil {
		t.Fatal(err)
	}
	request, found, err := local.LoadRequest(parent.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	adapter, err := sourceAdapter(testSources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(parent, adapter, archive.FilteredTranscript{Format: "codex-jsonl", Records: [][]byte{[]byte(`{"type":"session_meta","payload":{"id":"00000000-0000-0000-0000-000000000001","cwd":"/synthetic/project"}}`)}}, p.now, request.HookEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.LinkedSessions) != 1 || bundle.LinkedSessions[0].SessionID != "legitimate-unavailable" {
		t.Fatalf("ghost or unknown link incorrectly derived: %+v", bundle.LinkedSessions)
	}
	retained := false
	for _, item := range bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindLinkedSession && item.Provenance == ghost.Provenance && item.Payload["archive_session_id"] == oldID {
			retained = true
		}
	}
	if !retained {
		t.Fatal("raw historical composite observation deleted")
	}
	// A restart uses durable link-version evidence and performs no new session writes.
	regs, err := local.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	restarted := &pass{local: local, registrations: regs, now: at.Add(2 * time.Minute)}
	before := snapshotMtimes(t, local.Home())
	if err := restarted.reconcileNativeLinks(); err != nil || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatalf("legacy migration repeated writes after restart: %v", err)
	}
	if _, registered, err := local.LoadRegistration(oldID); err != nil || registered {
		t.Fatal("ghost was registered or state lookup failed", err)
	}
}
