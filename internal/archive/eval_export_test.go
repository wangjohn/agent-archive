package archive

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// evalExportSchema compiles schemas/eval-export.schema.json with the metadata
// schema it refers to.
func evalExportSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	var id string
	for _, name := range []string{"metadata.schema.json", "eval-export.schema.json"} {
		raw, err := os.ReadFile(filepath.Join(schemaDir, name))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		id, _ = doc.(map[string]any)["$id"].(string)
		if err := compiler.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// evalExportFixture is a handoff fixture's bundle, with a hook's final
// message and a feedback note, and its metadata as the collector would
// publish it for a registration that recorded commits and a replay marker.
func evalExportFixture(t *testing.T, harness string) (SourceBundle, Metadata) {
	t.Helper()
	bundle := handoffBundle(t, harness)
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	bundle.SupplementalEvidence = append(bundle.SupplementalEvidence,
		SupplementalEvidence{Kind: EvidenceKindFinalResponse, ObservedAt: at, Provenance: "hook:" + harness + ":stop", Payload: map[string]any{"text": "Final message the stop hook saw."}},
		SupplementalEvidence{Kind: EvidenceKindExplicitFeedback, ObservedAt: at, Provenance: "user:feedback", Payload: map[string]any{"text": "Right fix, but it should have added a test."}},
	)
	metadata, err := BuildMetadata(bundle, "machine-1", at.Add(-time.Hour), at, SourceReference{Key: "sessions/" + harness + "/s/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 10}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.StartHead = &GitHead{SHA: strings.Repeat("3f", 20), Dirty: new(true), ObservedAt: at.Add(-time.Hour)}
	reg.LastHead = &GitHead{SHA: strings.Repeat("9e", 20), ObservedAt: at}
	reg.Replay = &Replay{RunID: "run-1"}
	metadata.ApplyProjectName("/work/widget")
	metadata.ApplyRepoKey(RepoKey("https://example.test/acme/widget.git"))
	metadata.ApplyGitHead(reg)
	metadata.ApplyReplay(reg)
	return bundle, metadata
}

// Each harness's fixture exports to a checked-in golden, at both details:
// the contract the evaluation tool's own tests pin. Regenerate with
// `go test ./internal/archive -run TestEvalExportGolden -update` and review
// every changed line. Every line validates against the published schema.
func TestEvalExportGolden(t *testing.T) {
	schema := evalExportSchema(t)
	for _, harness := range []string{"claude", "codex", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			bundle, metadata := evalExportFixture(t, harness)
			var out bytes.Buffer
			for _, detail := range []EvalExportDetail{EvalExportDetailMetadata, EvalExportDetailFull} {
				// An archived session's metadata record is its sidecar's
				// alone, as eval export builds it.
				record := EvalExportFromMetadata(metadata, EvalExportSourceArchive)
				if detail == EvalExportDetailFull {
					var err error
					if record, err = BuildEvalExport(bundle, metadata, EvalExportSourceArchive, detail); err != nil {
						t.Fatal(err)
					}
				}
				line, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				validateAgainst(t, schema, harness+" "+string(detail), line)
				if err := ValidateEvalExport(record); err != nil {
					t.Errorf("%s %s: ValidateEvalExport rejects a valid record: %v", harness, detail, err)
				}
				out.Write(line)
				out.WriteByte('\n')
			}
			golden.Check(t, filepath.Join("testdata", "eval-export", harness+".jsonl"), out.Bytes())
		})
	}
}

// The full record holds every human prompt, in order and whole (a handoff
// cuts prompts; an export does not), and no harness-written record.
func TestEvalExportHasEveryPromptInOrder(t *testing.T) {
	t.Parallel()
	bundle, metadata := evalExportFixture(t, "claude")
	record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	if record.Prompts == nil || len(*record.Prompts) != 2 {
		t.Fatalf("prompts = %+v, want the two human prompts (the task notification is not one)", record.Prompts)
	}
	first := (*record.Prompts)[0]
	if first.Text != "Why does the widget test fail?" || first.Timestamp == "" {
		t.Errorf("first prompt = %+v", first)
	}
	if strings.Contains((*record.Prompts)[1].Text, "sk-abcdefghijklmnopqrstuv") {
		t.Error("an export carries a secret the filter redacted")
	}
	if record.FinalResponse == nil || record.FinalResponse.Source != "transcript" {
		t.Errorf("final response = %+v, want the transcript's last assistant text", record.FinalResponse)
	}
	if len(record.Feedback) != 1 || record.Replay == nil || record.GitHead == nil || record.Project.Root != "" || record.TranscriptPath != "" {
		t.Errorf("record = %+v", record)
	}
}

// The metadata record carries no conversation text, whatever the source
// holds, and can be built from a sidecar alone.
func TestEvalExportMetadataDetailHasNoConversationText(t *testing.T) {
	t.Parallel()
	bundle, metadata := evalExportFixture(t, "claude")
	fromSidecar, err := json.Marshal(EvalExportFromMetadata(metadata, EvalExportSourceArchive))
	if err != nil {
		t.Fatal(err)
	}
	record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailMetadata)
	if err != nil {
		t.Fatal(err)
	}
	withBundle, _ := json.Marshal(record)
	for _, line := range [][]byte{fromSidecar, withBundle} {
		for _, text := range []string{"Why does the widget test fail?", "Final message the stop hook saw.", "should have added a test", `"prompts"`, `"final_response"`, `"files_edited"`, `"feedback"`} {
			if bytes.Contains(line, []byte(text)) {
				t.Errorf("metadata record carries %q: %s", text, line)
			}
		}
	}
}

// With no assistant text in the transcript, the final response is what the
// stop hook reported.
func TestEvalExportFallsBackToTheHooksFinalMessage(t *testing.T) {
	t.Parallel()
	bundle, metadata := evalExportFixture(t, "claude")
	var kept []map[string]any
	for _, record := range bundle.NativeRecords {
		if record["type"] != "assistant" {
			kept = append(kept, record)
		}
	}
	bundle.NativeRecords = kept
	record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	if record.FinalResponse == nil || record.FinalResponse.Source != "hook" || record.FinalResponse.Text != "Final message the stop hook saw." {
		t.Errorf("final response = %+v", record.FinalResponse)
	}
}

func TestFitEvalExportCutsTextsNeverPrompts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 20000) // two bytes a rune: a cut must not split one
	prompts := []EvalPrompt{{Text: "short"}, {Text: long}, {Text: long + long}}
	files := []string{"a.go", "b.go", "c.go"}
	record := EvalExport{
		SchemaVersion: EvalExportSchemaVersion, Record: evalRecordSession, Source: EvalExportSourceArchive, Detail: EvalExportDetailFull,
		SessionID: "s", Prompts: &prompts, FinalResponse: &EvalFinalResponse{Text: long, Source: "transcript"}, FilesEdited: &files,
	}
	if got := FitEvalExport(record, 0); got.Trimmed != nil {
		t.Error("no bound cut something")
	}
	if got := FitEvalExport(record, 1<<20); got.Trimmed != nil {
		t.Error("a record under its bound was cut")
	}
	fitted := FitEvalExport(record, 20000)
	if size := evalSize(fitted); size > 20000 || fitted.Trimmed == nil || fitted.Trimmed.ExceedsMaxBytes {
		t.Fatalf("size %d, trimmed %+v", size, fitted.Trimmed)
	}
	if len(*fitted.Prompts) != 3 || (*fitted.Prompts)[0].Text != "short" || (*fitted.Prompts)[0].Truncated {
		t.Errorf("prompts = %+v, want all three and the short one whole", *fitted.Prompts)
	}
	for _, p := range *fitted.Prompts {
		if !utf8.ValidString(p.Text) {
			t.Error("a cut split a rune")
		}
	}
	if fitted.Trimmed.TextsTruncated != 3 || len(*fitted.FilesEdited) != 3 {
		t.Errorf("trimmed = %+v, files %v", fitted.Trimmed, *fitted.FilesEdited)
	}
	// The caller's record is left alone.
	if (*record.Prompts)[1].Text != long || record.FinalResponse.Truncated {
		t.Error("FitEvalExport changed its argument")
	}
	// A bound nothing can meet: texts at their floor, files dropped, and the
	// record says it is still over.
	tiny := FitEvalExport(record, 500)
	if !tiny.Trimmed.ExceedsMaxBytes || len(*tiny.Prompts) != 3 || len(*tiny.FilesEdited) != 0 || tiny.Trimmed.FilesEditedOmitted != 3 {
		t.Errorf("tiny = %+v, prompts %d, files %v", tiny.Trimmed, len(*tiny.Prompts), *tiny.FilesEdited)
	}
}

// An error record validates, and so does one with a code a later release
// might add.
func TestEvalExportErrorRecordMatchesTheSchema(t *testing.T) {
	t.Parallel()
	schema := evalExportSchema(t)
	record := NewEvalExportError(EvalExportSourceArchive, "not-an-id", EvalErrorNotFound, "no archived session with this ID")
	line, _ := json.Marshal(record)
	validateAgainst(t, schema, "error record", line)
	validateAgainst(t, schema, "future code", []byte(`{"schema_version":1,"record":"error","input":"x","error":{"code":"quota_exceeded","message":"m"}}`))
	instance, _ := jsonschema.UnmarshalJSON(strings.NewReader(`{"schema_version":1,"record":"error","input":"x","error":{"code":"x","message":"m"},"text":"leak"}`))
	if schema.Validate(instance) == nil {
		t.Error("an error record with an extra field validated")
	}
}

// A Cursor text transcript has prompts and a last reply but no records: the
// export reads its role sections, as handoff does.
func TestEvalExportReadsACursorTextTranscript(t *testing.T) {
	t.Parallel()
	filtered, err := CursorAdapter{}.FilterText(strings.NewReader("user:\nedit a.go\n\nassistant:\nworking on it\n\nuser:\n<user_query>\nand b.go\n</user_query>\n\nassistant:\ndone\n"), time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "cursor", CursorAdapter{}, filtered)
	record, err := BuildEvalExport(bundle, parserTestMetadata(t, bundle), EvalExportSourceLocal, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	if record.Prompts == nil || len(*record.Prompts) != 2 || (*record.Prompts)[0].Text != "edit a.go" || (*record.Prompts)[1].Text != "and b.go" {
		t.Errorf("prompts = %+v", record.Prompts)
	}
	if record.FinalResponse == nil || record.FinalResponse.Text != "done" || record.FilesEdited == nil || len(*record.FilesEdited) != 0 {
		t.Errorf("final response = %+v, files = %v", record.FinalResponse, record.FilesEdited)
	}
}

// A sidecar that decodes can still break the schema (a hand-edited or
// corrupted one): ValidateEvalExport rejects each such record the schema
// rejects, so eval export turns it into an error record instead.
func TestValidateEvalExportRejectsWhatTheSchemaRejects(t *testing.T) {
	t.Parallel()
	schema := evalExportSchema(t)
	bundle, metadata := evalExportFixture(t, "claude")
	valid, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	// Each case edits the record's JSON, as a hand-edited sidecar would be.
	for name, corrupt := range map[string]func(map[string]any){
		"negative count":       func(r map[string]any) { r["counts"].(map[string]any)["turns"] = -1 },
		"unknown parser state": func(r map[string]any) { r["parser"].(map[string]any)["status"] = "bogus" },
		"unknown state":        func(r map[string]any) { r["state"] = "bogus" },
		"unknown turn outcome": func(r map[string]any) { r["turn_outcome"] = "bogus" },
		"zero tool count":      func(r map[string]any) { r["tools_used"] = []any{map[string]any{"name": "Bash", "count": 0}} },
		"bad repo key":         func(r map[string]any) { r["project"].(map[string]any)["repo_key"] = "github.com/acme/widget" },
		"dirty last commit":    func(r map[string]any) { r["git_head"].(map[string]any)["last"].(map[string]any)["dirty"] = true },
		"bad replay run":       func(r map[string]any) { r["replay"] = map[string]any{"run_id": "has space"} },
		"bad git source": func(r map[string]any) {
			r["git_activity"] = []any{map[string]any{"kind": "commit", "source": "editor"}}
		},
		"negative model turns": func(r map[string]any) { r["models"].([]any)[0].(map[string]any)["turn_count"] = -2 },
	} {
		var raw map[string]any
		if err := json.Unmarshal(encoded, &raw); err != nil {
			t.Fatal(err)
		}
		corrupt(raw)
		line, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		if schema.Validate(instance) == nil {
			t.Errorf("%s: the schema accepts it, so this case tests nothing", name)
		}
		var record EvalExport
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if ValidateEvalExport(record) == nil {
			t.Errorf("%s: ValidateEvalExport accepts a record the schema rejects", name)
		}
	}
}

// A sidecar written by an older parser does not lend its counts and parser
// to a full record whose prompts come from this build's parse: both halves
// describe the same parse. What the hooks recorded stays the sidecar's.
func TestEvalExportFullRecordUsesOneParser(t *testing.T) {
	t.Parallel()
	bundle, current := evalExportFixture(t, "claude")
	old := current
	old.Parser = ParserInfo{Name: current.Parser.Name, Version: "0.1.0", Status: ParserStatusPartial}
	old.Counts.Turns, old.ToolsUsed = new(99), nil
	record, err := BuildEvalExport(bundle, old, EvalExportSourceArchive, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	if record.Parser != current.Parser || *record.Counts.Turns != *current.Counts.Turns || !slices.Equal(record.ToolsUsed, current.ToolsUsed) {
		t.Errorf("parser %+v, turns %d, tools %v; want this build's %+v, %d, %v", record.Parser, *record.Counts.Turns, record.ToolsUsed, current.Parser, *current.Counts.Turns, current.ToolsUsed)
	}
	if record.GitHead != old.GitHead || record.Replay != old.Replay || record.Project.RepoKey != old.RepoKey {
		t.Error("re-deriving dropped what the hooks recorded")
	}
	// The metadata record is the sidecar's alone, old parser and all.
	if meta := EvalExportFromMetadata(old, EvalExportSourceArchive); meta.Parser.Version != "0.1.0" {
		t.Errorf("metadata record parser = %+v", meta.Parser)
	}
}

// A subagent's final message in its parent's bundle is never the parent's
// final response.
func TestEvalExportIgnoresASubagentsHookFinal(t *testing.T) {
	t.Parallel()
	bundle, metadata := evalExportFixture(t, "claude")
	var kept []map[string]any
	for _, record := range bundle.NativeRecords {
		if record["type"] != "assistant" {
			kept = append(kept, record)
		}
	}
	bundle.NativeRecords = kept
	bundle.SupplementalEvidence = append(bundle.SupplementalEvidence, SupplementalEvidence{
		Kind: EvidenceKindFinalResponse, ObservedAt: time.Date(2026, 9, 22, 12, 30, 0, 0, time.UTC), Provenance: "hook:claude:SubagentStop",
		Payload: map[string]any{"text": "The subagent's answer.", "agent_id": "agent-1"},
	})
	record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailFull)
	if err != nil {
		t.Fatal(err)
	}
	if record.FinalResponse == nil || record.FinalResponse.Text != "Final message the stop hook saw." {
		t.Errorf("final response = %+v, want the parent's own", record.FinalResponse)
	}
}

// A session started on a detached checkout, or with a malformed branch,
// exports no starting branch rather than a made-up one.
func TestEvalExportStartingBranchIsValidated(t *testing.T) {
	t.Parallel()
	bundle, metadata := evalExportFixture(t, "claude")
	for recorded, want := range map[string]string{"HEAD": "", "bad branch name": "", "fix/widget-test": "fix/widget-test"} {
		b := bundle
		b.NativeRecords = append([]map[string]any{{"type": "system", "gitBranch": recorded}}, bundle.NativeRecords...)
		record, err := BuildEvalExport(b, metadata, EvalExportSourceArchive, EvalExportDetailMetadata)
		if err != nil {
			t.Fatal(err)
		}
		if record.Branch != want {
			t.Errorf("recorded %q: branch %q, want %q", recorded, record.Branch, want)
		}
	}
}
