package collector

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestLegacyPreservedProvenanceUsesOriginalParent(t *testing.T) {
	for _, missing := range []string{"filter", "schema", "both"} {
		for _, originalParent := range []string{"", "conflicting-known-parent"} {
			name := missing + "/unresolved"
			if originalParent != "" {
				name = missing + "/known-conflict"
			}
			t.Run(name, func(t *testing.T) {
				scan, p := privacyJournal(t)
				if len(p.History.Inputs) < 2 {
					t.Fatal("fixture needs an independently retained preserved original")
				}
				input := p.History.Inputs[1]
				raw, err := scan.local.ReadPendingSource(scan.id(), state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
				if err != nil {
					t.Fatal(err)
				}
				original, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				original.NativeChild, original.ParentSessionID = true, originalParent
				packed, err := archive.BuildCompressedSource(original)
				if err != nil {
					t.Fatal(err)
				}
				key, err := archive.SourceObjectKey(original, packed.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
				if err := scan.remote.Put(t.Context(), key, packed.Bytes); err != nil {
					t.Fatal(err)
				}
				var final archive.Metadata
				if err := json.Unmarshal(p.MetadataBytes, &final); err != nil {
					t.Fatal(err)
				}
				final.ParentSessionID, final.NativeChild = "qualified-late-parent", true
				scan.reg.ParentSessionID, scan.reg.NativeChild = final.ParentSessionID, true
				revision := archive.RevisionReference{RevisionID: input.RevisionID, CapturedAt: original.Capture.CapturedAt, Source: ref, FilterVersion: original.Capture.FilterVersion, SourceSchemaVersion: original.SchemaVersion}
				if missing == "filter" || missing == "both" {
					revision.FilterVersion = ""
				}
				if missing == "schema" || missing == "both" {
					revision.SourceSchemaVersion = 0
				}
				final.History.Preserved = []archive.RevisionReference{revision}
				scan.revisions = &revisionPlan{Preserved: final.History.Preserved}
				history := &state.PendingHistory{Version: 1}
				err = scan.freezePreservedHistoryInputs(&final, history)
				if originalParent != "" {
					if err == nil {
						t.Fatal("different known original parent was accepted")
					}
					return
				}
				if err != nil {
					t.Fatal("legacy original unresolved child rejected before header freeze", err)
				}
				if len(history.Inputs) != 1 || history.Inputs[0].Reference != ref || history.Inputs[0].ParentSessionID == nil || *history.Inputs[0].ParentSessionID != "" || history.Inputs[0].NativeChild == nil || !*history.Inputs[0].NativeChild {
					t.Fatal("original header authority was replaced by final parent", history.Inputs)
				}
				derived := final.History.Preserved[0]
				if derived.FilterVersion != original.Capture.FilterVersion || derived.SourceSchemaVersion != original.SchemaVersion || history.Inputs[0].FilterVersion != derived.FilterVersion || history.Inputs[0].SourceSchemaVersion != derived.SourceSchemaVersion {
					t.Fatal("legacy provenance was not derived from checksum-bound original")
				}
			})
		}
	}
}
