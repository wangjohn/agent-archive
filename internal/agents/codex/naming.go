package codex

import (
	"bytes"
	"encoding/json"
	"maps"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// NamingOnlyChange excludes only verified owning label evidence and capture
// bookkeeping. Any change to native content, identity, harness or links is activity.
func (Filter) NamingOnlyChange(previous, candidate archive.SourceBundle) bool {
	if previous.History != nil || candidate.History != nil || previous.SchemaVersion != archive.SourceSchemaVersion || candidate.SchemaVersion != archive.SourceSchemaVersion || previous.Capture.Harness.Name != "codex" || candidate.Capture.Harness.Name != "codex" || archive.ValidateSessionLabels(previous) != nil || archive.ValidateSessionLabels(candidate) != nil {
		return false
	}
	before, _, _ := archive.CurrentSessionLabel(previous)
	after, _, _ := archive.CurrentSessionLabel(candidate)
	if before.Fingerprint() == after.Fingerprint() {
		return false
	}

	a, errA := json.Marshal(normalizeNamingBundle(previous, previous, candidate, false))
	b, errB := json.Marshal(normalizeNamingBundle(candidate, previous, candidate, true))
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func normalizeNamingBundle(bundle, previous, candidate archive.SourceBundle, upgraded bool) archive.SourceBundle {
	bundle.Capture.AdapterVersion = ""
	bundle.Capture.FilterVersion = ""
	bundle.Capture.CapturedAt = time.Time{}
	bundle.Capture.Boundary = archive.CaptureBoundary{}
	bundle.Capture.Gaps = nil
	// The enum newly retained by filter17 describes storage, not a
	// conversation event. Copy maps before removing it for comparison.
	bundle.NativeRecords = append([]map[string]any(nil), bundle.NativeRecords...)
	for i, record := range bundle.NativeRecords {
		if !upgraded || previous.Capture.FilterVersion == archive.FilterVersion || candidate.Capture.FilterVersion != archive.FilterVersion || i >= len(previous.NativeRecords) || record["type"] != "session_meta" {
			continue
		}
		oldPayload, ok := previous.NativeRecords[i]["payload"].(map[string]any)
		if !ok || previous.NativeRecords[i]["type"] != "session_meta" {
			continue
		}
		if _, explicit := oldPayload["history_mode"]; explicit {
			continue
		}
		payload, ok := record["payload"].(map[string]any)
		if !ok {
			continue
		}
		mode, hasMode := payload["history_mode"]
		if !hasMode || (mode != "legacy" && mode != "paginated") {
			continue
		}
		copyRecord := make(map[string]any, len(record))
		maps.Copy(copyRecord, record)
		copyPayload := make(map[string]any, len(payload))
		for key, value := range payload {
			if key != "history_mode" {
				copyPayload[key] = value
			}
		}
		copyRecord["payload"] = copyPayload
		bundle.NativeRecords[i] = copyRecord
	}
	evidenceItems := bundle.SupplementalEvidence
	bundle.SupplementalEvidence = nil
	for _, evidence := range evidenceItems {
		if evidence.Kind != archive.EvidenceKindSessionLabels {
			bundle.SupplementalEvidence = append(bundle.SupplementalEvidence, evidence)
		}
	}
	if len(bundle.NativeRecords) == 0 {
		bundle.NativeRecords = nil
	}
	return bundle
}
