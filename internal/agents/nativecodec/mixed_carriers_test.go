package nativecodec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPersistedMixedCarriersRetainNativeEvidence(t *testing.T) {
	t.Parallel()
	original := handoffBundle(t, "claude")
	oldH, err := BuildHandoff(original, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ref := SourceReference{Key: "synthetic", SHA256: strings.Repeat("a", 64)}
	oldM, err := BuildMetadata(original, "synthetic", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := (CursorAdapter{}).FilterText(strings.NewReader("user: Supplemental visible prompt\n\nassistant: Supplemental visible answer\n"), now)
	if err != nil {
		t.Fatal(err)
	}
	filtered := FilteredTranscript{Format: original.Capture.SourceFormat, Text: text.Text}
	for _, record := range original.NativeRecords {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		filtered.Records = append(filtered.Records, raw)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	mixed, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := BuildCompressedSource(mixed)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := ReadSourceBundle(bytes.NewReader(compressed.Bytes), DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.NativeRecords) == 0 || len(persisted.NativeText) == 0 {
		t.Fatal("did not persist mixed evidence")
	}
	h, err := BuildHandoff(persisted, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := BuildMetadata(persisted, "synthetic", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	tools := func(h Handoff) int {
		n := 0
		for _, ex := range h.Exchanges {
			for _, s := range ex.Steps {
				if s.Tool != nil {
					n++
				}
			}
		}
		return n
	}
	t.Logf("records=%d text=%d baseline(files=%d plan=%d tools=%d models=%d) mixed(files=%d plan=%d tools=%d models=%d)", len(persisted.NativeRecords), len(persisted.NativeText), len(oldH.FilesTouched), len(oldH.Plan), tools(oldH), len(oldM.Models), len(h.FilesTouched), len(h.Plan), tools(h), len(m.Models))
	if len(h.FilesTouched) != len(oldH.FilesTouched) {
		t.Errorf("retained native touched files lost: %v => %v", oldH.FilesTouched, h.FilesTouched)
	}
	if len(h.Plan) != len(oldH.Plan) {
		t.Errorf("retained native plan lost: %d => %d", len(oldH.Plan), len(h.Plan))
	}
	if tools(h) != tools(oldH) {
		t.Errorf("retained native tool calls lost: %d => %d", tools(oldH), tools(h))
	}
	if len(m.Models) != len(oldM.Models) {
		t.Errorf("retained native models lost: %d => %d", len(oldM.Models), len(m.Models))
	}

	if !reflect.DeepEqual(h.Exchanges, oldH.Exchanges) || !reflect.DeepEqual(h.Plan, oldH.Plan) || !reflect.DeepEqual(h.FilesTouched, oldH.FilesTouched) || !reflect.DeepEqual(m.Models, oldM.Models) {
		t.Error("mixed carriers changed retained structured semantics")
	}
	if m.Counts.Turns != nil || m.Counts.ToolCalls != nil {
		t.Error("mixed text made structured counts provable")
	}
	if m.Title != oldM.Title {
		t.Errorf("structured title changed: %q => %q", oldM.Title, m.Title)
	}
}
