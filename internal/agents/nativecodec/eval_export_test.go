package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/archive"
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
				out.Write(line)
				out.WriteByte('\n')
			}
			golden.Check(t, filepath.Join("..", "..", "archive", "testdata", "eval-export", harness+".jsonl"), out.Bytes())
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

// A local record: the transcript as the collector filters it, summarized with
// the running parser, validated against the schema and pinned by a golden.
// It names the app's own session ID and the transcript's path, and leaves out
// what only a hook or the archive records.
func TestLocalEvalExportGolden(t *testing.T) {
	schema := evalExportSchema(t)
	bundle := handoffBundle(t, "claude")
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	for _, detail := range []EvalExportDetail{EvalExportDetailMetadata, EvalExportDetailFull} {
		record, err := BuildLocalEvalExport(bundle, LocalTranscript{Path: "/Users/someone/.claude/projects/widget/native-456.jsonl", Now: now}, detail)
		if err != nil {
			t.Fatal(err)
		}
		if record.SessionID != bundle.NativeSessionID || record.GitHead != nil || record.Replay != nil || record.MachineID != "" || record.CapturedAt != nil || record.Project.Root == "" {
			t.Errorf("%s record = %+v", detail, record)
		}
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		validateAgainst(t, schema, "local "+string(detail), line)
		out.Write(line)
		out.WriteByte('\n')
	}
	golden.Check(t, filepath.Join("..", "..", "archive", "testdata", "eval-export", "local-claude.jsonl"), out.Bytes())
}

// A transcript whose records carry no time has no started_at, unless
// discovery supplied one: the export time is never passed off as the start.
func TestLocalEvalExportNeverInventsAStartTime(t *testing.T) {
	t.Parallel()
	filtered, err := CursorAdapter{}.FilterText(strings.NewReader("user:\nedit a.go\n\nassistant:\ndone\n"), time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "cursor", CursorAdapter{}, filtered)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	record, err := BuildLocalEvalExport(bundle, LocalTranscript{Path: "/t.txt", Now: now}, EvalExportDetailMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if record.StartedAt != nil {
		t.Errorf("started_at = %v, want none", record.StartedAt)
	}
	created := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	record, err = BuildLocalEvalExport(bundle, LocalTranscript{Path: "/t.txt", StartedAt: created, Now: now}, EvalExportDetailMetadata)
	if err != nil || record.StartedAt == nil || !record.StartedAt.Equal(created) {
		t.Errorf("started_at = %v (%v), want discovery's %v", record.StartedAt, err, created)
	}
}

// Hook evidence belongs to the archive even when a caller supplies an archived bundle.
func TestLocalEvalExportIgnoresArchiveEvidence(t *testing.T) {
	t.Parallel()
	bundle, _ := evalExportFixture(t, "codex")
	for _, detail := range []EvalExportDetail{EvalExportDetailMetadata, EvalExportDetailFull} {
		record, err := BuildLocalEvalExport(bundle, LocalTranscript{Path: "/native.jsonl", Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}, detail)
		if err != nil {
			t.Fatal(err)
		}
		if (record.Counts.ExplicitFeedback != nil && *record.Counts.ExplicitFeedback != 0) || len(record.Feedback) != 0 || record.Replay != nil || record.GitHead != nil {
			t.Fatalf("%s carries archive evidence: %+v", detail, record)
		}
	}
}

// Export fixtures exercise native decoding; the production builders receive typed analysis.
type EvalExport = archive.EvalExport

type EvalExportDetail = archive.EvalExportDetail

type EvalExportSource = archive.EvalExportSource

type EvalPrompt = archive.EvalPrompt

type EvalFinalResponse = archive.EvalFinalResponse

type EvalFeedback = archive.EvalFeedback

type LocalTranscript = archive.LocalTranscript

const EvalExportSchemaVersion = archive.EvalExportSchemaVersion

const EvalExportDetailMetadata = archive.EvalExportDetailMetadata

const EvalExportDetailFull = archive.EvalExportDetailFull

const EvalExportSourceArchive = archive.EvalExportSourceArchive

const EvalExportSourceLocal = archive.EvalExportSourceLocal

const EvalErrorNotFound = archive.EvalErrorNotFound

var EvalExportFromMetadata = archive.EvalExportFromMetadata

var NewEvalExportError = archive.NewEvalExportError

var FitEvalExport = archive.FitEvalExport

func BuildEvalExport(b SourceBundle, m Metadata, source EvalExportSource, detail EvalExportDetail) (EvalExport, error) {
	a, err := Parse(context.Background(), b)
	if err != nil {
		return EvalExport{}, err
	}
	return archive.BuildEvalExportWithAnalysis(b, a, m, source, detail)
}

func BuildLocalEvalExport(b SourceBundle, local LocalTranscript, detail EvalExportDetail) (EvalExport, error) {
	b.SupplementalEvidence = nil
	a, err := Parse(context.Background(), b)
	return archive.BuildLocalEvalExportWithAnalysis(b, a, err, local, detail, ParserInfo{})
}
