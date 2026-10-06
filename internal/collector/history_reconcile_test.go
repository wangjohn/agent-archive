package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const revisionThread = "11111111-1111-4111-8111-111111111111"

const revisionB = "22222222-2222-4222-8222-222222222222"

const revisionC = "33333333-3333-4333-8333-333333333333"

func revisionBundle(t *testing.T, id string, texts ...string) archive.SourceBundle {
	t.Helper()
	records := []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": revisionThread}}}
	ordinals := []uint64{0}
	for i, text := range texts {
		records = append(records, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}})
		ordinals = append(ordinals, uint64(i+1))
	}
	return archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: "synthetic", NativeSessionID: revisionThread, ProjectID: "project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "test", SourceFormat: "codex-jsonl", FilterVersion: "test", CapturedAt: time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)}, NativeRecords: records, Ordinals: ordinals, History: &archive.SourceHistory{ThreadID: revisionThread, ActiveRolloutID: id, Spans: []archive.HistorySpan{{ThreadID: revisionThread, RolloutID: id, EndRecord: len(records), EndOrdinal: uint64(len(records))}}}}
}

func TestRevisionPlannerFirstABCAndOutgoingAppend(t *testing.T) {
	a := revisionBundle(t, revisionThread, "common", "A-only")
	b := revisionBundle(t, revisionB, "common", "B-only")
	c := revisionBundle(t, revisionC, "common", "C-only")
	plan := &revisionPlan{Current: revisionC, ObservedAt: c.Capture.CapturedAt}
	planner := revisionPlanner{plan: plan, active: c, adapter: codex.Filter{}, bytesLeft: 128 << 20}
	for _, bundle := range []archive.SourceBundle{b, a, b} {
		if err := planner.add(bundle); err != nil {
			t.Fatal(err)
		}
	}
	if len(plan.Preserved) != 2 || len(plan.Sources) != 2 {
		t.Fatalf("first import lost alternatives: %#v", plan)
	}
	// A never-published append replaces B's physical entry, retaining its suffix.
	appended := revisionBundle(t, revisionB, "common", "B-only", "unobserved")
	appended.Capture.CapturedAt = plan.ObservedAt.Add(time.Minute)
	if err := planner.add(appended); err != nil {
		t.Fatal(err)
	}
	if len(plan.Preserved) != 2 || len(plan.Sources) != 2 || !plan.MeaningfulAt.Equal(appended.Capture.CapturedAt) {
		t.Fatal("append did not update one physical entry")
	}
	for _, stage := range plan.Sources {
		decoded, err := archive.ReadSourceBundle(bytes.NewReader(stage.Bytes), archive.DecodeOptions{})
		if err != nil || !ownedEvidenceCovered(codex.Filter{}, stage.Bundle, decoded) {
			t.Fatal("staged evidence does not read back", err)
		}
	}
	if len(plan.historyInputs()) != 2 {
		t.Fatal("missing frozen input seam")
	}
}

func TestRevisionPlannerOwnershipReactivationAndSameSizeRewrite(t *testing.T) {
	active := revisionBundle(t, revisionC, "prefix")
	old := revisionBundle(t, revisionB, "prefix", "owned")
	own := uint64(3)
	old.History.OwnStart = &own // All text is inherited; it is not history activity.
	p := revisionPlanner{plan: &revisionPlan{Current: revisionC}, active: active, adapter: codex.Filter{}, bytesLeft: 128 << 20}
	if err := p.add(old); err != nil || len(p.plan.Preserved) != 0 {
		t.Fatal("copied prefix preserved", err)
	}
	old.History.OwnStart = nil
	if err := p.add(old); err != nil {
		t.Fatal(err)
	}
	rewrite := revisionBundle(t, revisionB, "prefix", "other") // Same count and string length.
	if err := p.add(rewrite); !agentapi.HasFailure(err, agentapi.Changed) {
		t.Fatal("same-size contradiction accepted", err)
	}
	p.plan.Current = revisionB
	if err := p.add(old); err != nil || len(p.plan.Sources) != 1 {
		t.Fatal("reactivation added another source", err)
	}
	// An ordinary append on the active physical revision never adds a history entry.
	activeAppend := revisionBundle(t, revisionC, "prefix", "append")
	p = revisionPlanner{plan: &revisionPlan{Current: revisionC}, active: activeAppend, adapter: codex.Filter{}, bytesLeft: 128 << 20}
	if err := p.add(active); err != nil || len(p.plan.Preserved) != 0 {
		t.Fatal("active append preserved", err)
	}
}

func TestRevisionPlannerBoundsAndFilterChangesCannotProveCoverage(t *testing.T) {
	b, c := revisionBundle(t, revisionB, "lost"), revisionBundle(t, revisionC, "current")
	p := revisionPlanner{plan: &revisionPlan{Current: revisionC}, active: c, adapter: codex.Filter{}, bytesLeft: 1}
	if err := p.add(b); !agentapi.HasFailure(err, agentapi.Limit) || len(p.plan.Preserved) != 0 {
		t.Fatal("stage budget not enforced", err)
	}
	c = b
	c.Capture.FilterVersion = "new"
	if ownedEvidenceCovered(codex.Filter{}, b, c) {
		t.Fatal("filter change manufactured coverage")
	}
	raw, _ := json.Marshal(b)
	var restored archive.SourceBundle
	if err := json.Unmarshal(raw, &restored); err != nil || !ownedEvidenceCovered(codex.Filter{}, b, restored) {
		t.Fatal("restart changed ordinal coverage", err)
	}
	if errors.Is(p.add(b), archive.ErrHistoryMutationPending) {
		t.Fatal("planner must produce evidence independently of publication fence")
	}
}
