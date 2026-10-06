package collector

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestExternalRenameVerifiesWholeRetainedSourceBeforePublication(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	provider := &mutableLabels{}
	opts := Options{Sources: testSources, Parsers: testParsers, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	file := readPublishedStateFile(t, local, reg.ArchiveSessionID)
	file.Bundle.NativeRecords = append(file.Bundle.NativeRecords, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Uncommitted retained bytes"}})
	writePublishedStateFile(t, local, reg.ArchiveSessionID, file)
	provider.label = archive.SessionLabel{State: "present", Name: "New name", Source: "database", Contract: archive.SessionLabelContract}
	now = now.Add(time.Hour)
	remote.keys = nil
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Published) != 0 || result.Errors[reg.ArchiveSessionID] == nil || len(remote.keys) != 0 {
		t.Fatalf("unverified retained bytes were published: %+v %v %v", result, err, remote.keys)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle != before.SourceBundle || after.Name != before.Name {
		t.Fatal("failed source verification changed archived metadata")
	}
}
