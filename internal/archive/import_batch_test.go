package archive

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Registrations as main wrote them before ImportBatch had a type of its own
// (json.MarshalIndent, as local.Write does): an import with an ID, and a
// hook registration without one. They must read the same and write back
// byte for byte.
const (
	mainImportRegistration = `{
  "archive_session_id": "0123456789abcdef0123456789abcdef",
  "native_session_id": "native",
  "project_id": "p",
  "project_root": "/p",
  "harness": {
    "name": "codex",
    "version": "1.2.3"
  },
  "transcript_path": "/p/t.jsonl",
  "session_started_at": "2026-09-23T10:00:00Z",
  "registered_at": "2026-09-23T10:00:00Z",
  "subagent_observed_at": "0001-01-01T00:00:00Z",
  "admitted_at": "2026-09-23T10:00:00Z",
  "origin": "import",
  "started_at_source": "transcript",
  "import_batch": "2026-09-23-1",
  "destination_id": "dest"
}`
	mainHookRegistration = `{
  "archive_session_id": "fedcba9876543210fedcba9876543210",
  "native_session_id": "hook",
  "project_id": "p",
  "project_root": "/p",
  "harness": {
    "name": "claude"
  },
  "transcript_path": "/p/h.jsonl",
  "session_started_at": "2026-09-23T10:00:00Z",
  "registered_at": "2026-09-23T10:00:00Z",
  "subagent_observed_at": "0001-01-01T00:00:00Z",
  "admitted_at": "2026-09-23T10:00:00Z",
  "origin": "hook",
  "started_at_source": "hook",
  "destination_id": "dest"
}`
)

func TestImportBatchKeepsTheOnDiskFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		batch string
	}{
		{"import", mainImportRegistration, "2026-09-23-1"},
		{"hook", mainHookRegistration, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reg SessionRegistration
			if err := json.Unmarshal([]byte(tc.file), &reg); err != nil {
				t.Fatal(err)
			}
			if reg.ImportBatch.Recorded() != tc.batch || reg.ImportBatch.IsZero() != (tc.batch == "") {
				t.Fatalf("import batch %q, want %q", reg.ImportBatch.Recorded(), tc.batch)
			}
			if reg.InBatch("2026-09-23-1") != (tc.batch != "") {
				t.Fatalf("InBatch = %v", reg.InBatch("2026-09-23-1"))
			}
			out, err := json.MarshalIndent(reg, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.file {
				t.Fatalf("written back differently:\n%s\nwant:\n%s", out, tc.file)
			}
		})
	}
	// null and a missing field read as no import.
	for _, file := range []string{`{"import_batch":null}`, `{}`} {
		var reg SessionRegistration
		if err := json.Unmarshal([]byte(file), &reg); err != nil || !reg.ImportBatch.IsZero() {
			t.Fatalf("%s: %+v %v", file, reg.ImportBatch, err)
		}
	}
	var reg SessionRegistration
	if err := json.Unmarshal([]byte(`{"import_batch":7}`), &reg); err == nil {
		t.Fatal("a number read as an import ID")
	}
}

// Regression: phase-1 review follow-up to B-23. The InBatch guard could be
// bypassed by copying reg.ImportBatch into a variable and comparing that.
// The type now cannot be compared at all, so such code does not compile;
// this pins that, since nothing else would notice the blank func field
// going away.
func TestImportBatchCannotBeCompared(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[ImportBatch](), reflect.TypeFor[SessionRegistration]()} {
		if typ.Comparable() {
			t.Errorf("%s is comparable, so an import ID can be matched with == outside InBatch", typ)
		}
	}
}

func TestInBatchNeedsAnImportAndAnID(t *testing.T) {
	imported := SessionRegistration{Origin: SessionOriginImport, ImportBatch: NewImportBatch("2026-09-23-1")}
	hook := SessionRegistration{Origin: SessionOriginHook, ImportBatch: NewImportBatch("2026-09-23-1")}
	unbatched := SessionRegistration{Origin: SessionOriginImport}
	for _, tc := range []struct {
		reg  SessionRegistration
		id   string
		want bool
	}{
		{imported, "2026-09-23-1", true},
		{imported, "2026-09-23-2", false},
		{imported, "", false},
		{hook, "2026-09-23-1", false},
		{unbatched, "", false},
		{SessionRegistration{}, "", false},
	} {
		if got := tc.reg.InBatch(tc.id); got != tc.want {
			t.Errorf("InBatch(%+v, %q) = %v", tc.reg, tc.id, got)
		}
	}
}
