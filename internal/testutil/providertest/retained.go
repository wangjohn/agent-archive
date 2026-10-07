package providertest

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RetainedFixture contains real encoded synthetic source envelopes, never a
// claim about installed app layouts or raw native dependency continuity.
type RetainedFixture struct {
	Registration archive.SessionRegistration
	Metadata     archive.Metadata
	Body         []byte
	Active       archive.SourceBundle
	Unreferenced archive.SourceReference
}

// PutRetainedFixture creates a complete current/preserved set and one unrelated
// source through actual archive codecs. Callers must verify selection/readback
// through the production ports they are accepting.
func PutRetainedFixture(t *testing.T, remote storage.ObjectStore, preserved int, at time.Time) RetainedFixture {
	t.Helper()
	if preserved < 0 || preserved > archive.MaxPreservedRevisions {
		t.Fatal("invalid synthetic retained fixture size")
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NativeSessionID: "11111111-1111-4111-8111-111111111111",
		ProjectID: "synthetic-provider-project", ProjectRoot: "/synthetic/provider-project",
		Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at,
	}
	var active archive.SourceBundle
	var current archive.SourceReference
	var revisions []archive.RevisionReference
	var unreferenced archive.SourceReference
	for i := range preserved + 2 {
		revision := fmt.Sprintf("%08x-1111-4111-8111-%012x", i+1, i+1)
		captured := at.Add(-time.Duration(i) * time.Minute)
		bundle := archive.SourceBundle{
			SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID,
			NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID,
			Capture:  archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "synthetic-provider", FilterVersion: archive.FilterVersion, SourceFormat: "codex-jsonl", CapturedAt: captured},
			History:  &archive.SourceHistory{ThreadID: reg.NativeSessionID, ActiveRolloutID: revision, Spans: []archive.HistorySpan{{RolloutID: revision, ThreadID: reg.NativeSessionID, StartOrdinal: 0, EndOrdinal: 2, EndRecord: 2}}},
			Ordinals: []uint64{0, 1}, NativeRecords: []map[string]any{
				{"type": "session_meta", "payload": map[string]any{"id": reg.NativeSessionID, "cwd": reg.ProjectRoot, "history_mode": "paginated"}},
				{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": fmt.Sprintf("synthetic retained provider content %d", i)}},
			},
		}
		encoded, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(bundle, encoded.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		ref := archive.SourceReference{Key: key, SHA256: encoded.SHA256, CompressedBytes: len(encoded.Bytes)}
		if err := remote.Put(t.Context(), key, encoded.Bytes); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			active, current = bundle, ref
		case preserved + 1:
			unreferenced = ref
		default:
			revisions = append(revisions, archive.RevisionReference{RevisionID: revision, CapturedAt: captured, Source: ref})
		}
	}
	metadata, err := archive.BuildMetadataWithAnalysis(active, archive.Analysis{}, nil, "synthetic-provider-owner", reg.SessionStartedAt, at, current, archive.ParserInfo{Version: archive.DefaultParserVersion})
	if err != nil {
		t.Fatal(err)
	}
	metadata.History.Preserved = revisions
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	sources := make([]storage.SourcePublication, len(refs))
	for i, ref := range refs {
		sources[i] = storage.SourcePublication{Key: ref.Key, SHA256: ref.SHA256, Size: ref.CompressedBytes}
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, body, storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(t.Context(), remote, key, body); err != nil {
		t.Fatal(err)
	}
	return RetainedFixture{Registration: reg, Metadata: metadata, Body: body, Active: active, Unreferenced: unreferenced}
}
