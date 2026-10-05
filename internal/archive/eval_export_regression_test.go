package archive

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFitEvalExportMeasuresDisplayedJSON(t *testing.T) {
	prompts := []EvalPrompt{{Text: strings.Repeat("\u009b", 1000)}}
	record := EvalExport{Prompts: &prompts}
	got := FitEvalExport(record, 3000)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if size := len(DisplayJSON(raw)); size > 3000 && (got.Trimmed == nil || !got.Trimmed.ExceedsMaxBytes) {
		t.Fatalf("emitted %d bytes without exceeding flag", size)
	}
}

func TestFitEvalExportKeepsWholeRuneTextFloor(t *testing.T) {
	prompts := []EvalPrompt{{Text: strings.Repeat("界", 200)}}
	got := FitEvalExport(EvalExport{Prompts: &prompts}, 1)
	if n := len((*got.Prompts)[0].Text); n < minEvalText || n > minEvalText+3 {
		t.Fatalf("floor is %d bytes", n)
	}
}

func TestFitEvalExportSkipsIndivisibleFloorAndFitsOtherText(t *testing.T) {
	prompts := []EvalPrompt{{Text: strings.Repeat("a", 255) + "界"}, {Text: strings.Repeat("b", 257)}}
	got := FitEvalExport(EvalExport{Prompts: &prompts}, 1)
	if (*got.Prompts)[0].Text != prompts[0].Text || len((*got.Prompts)[1].Text) != 256 || got.Trimmed.TextsTruncated != 1 || !got.Trimmed.ExceedsMaxBytes {
		t.Fatalf("prompts %+v trimmed %+v", *got.Prompts, got.Trimmed)
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
