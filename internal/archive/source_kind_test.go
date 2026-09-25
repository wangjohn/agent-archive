package archive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionRegistrationSourceKind(t *testing.T) {
	base := SessionRegistration{
		ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", ProjectRoot: "/p",
		Harness: Harness{Name: "cursor"}, SessionStartedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	file := base
	if err := file.Validate(); err != nil || !file.ReadsTranscriptFile() {
		t.Fatalf("a file session without a path: %v", err)
	}
	data, _ := json.Marshal(file)
	if strings.Contains(string(data), "source_") {
		t.Fatalf("a file registration carries source fields: %s", data)
	}

	chat := base
	chat.SourceKind, chat.SourceKey = SourceKindCursorSQLite, "n"
	if err := chat.Validate(); err != nil || chat.ReadsTranscriptFile() {
		t.Fatalf("a Cursor database session: %v", err)
	}
	data, _ = json.Marshal(chat)
	if !strings.Contains(string(data), `"source_kind":"cursor-sqlite","source_key":"n"`) {
		t.Fatalf("%s", data)
	}

	for name, mutate := range map[string]func(*SessionRegistration){
		"no source key": func(r *SessionRegistration) { r.SourceKey = " " },
		"a path":        func(r *SessionRegistration) { r.TranscriptPath = "/t/n.jsonl" },
		"another key":   func(r *SessionRegistration) { r.SourceKey = "other" },
		"not cursor":    func(r *SessionRegistration) { r.Harness.Name = "codex" },
		//lint:ignore LV1001 deliberately a source kind with no declared constant
		"an unknown kind": func(r *SessionRegistration) { r.SourceKind = "cursor-leveldb" },
	} {
		r := chat
		mutate(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
