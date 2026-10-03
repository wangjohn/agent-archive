package archive

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestEvalExportParserRefreshPreservesAdmissionGaps(t *testing.T) {
	t.Parallel()
	for _, origin := range []SessionOrigin{SessionOriginImport, SessionOriginDiscovery} {
		t.Run(string(origin), func(t *testing.T) {
			bundle, metadata := evalExportFixture(t, "claude")
			metadata.ApplyRegistrationProvenance(SessionRegistration{Origin: origin})
			metadata.Parser.Version = "0.1.0"
			record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, EvalExportDetailFull)
			if err != nil {
				t.Fatal(err)
			}
			for _, gap := range metadata.CaptureGaps {
				if (gap.Code == CaptureGapImportedWithoutHookEvidence || gap.Code == CaptureGapDiscoveredWithoutHookEvidence) && !slices.Contains(record.CaptureGaps, gap) {
					t.Errorf("parser refresh dropped admission gap %+v; retained %+v", gap, record.CaptureGaps)
				}
			}
		})
	}
}

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

func TestEvalExportBranchExcludesParentSidechain(t *testing.T) {
	bundle := SourceBundle{NativeRecords: []map[string]any{{"isSidechain": true, "gitBranch": "child"}, {"gitBranch": "parent"}}}
	if got := firstBranch(bundle); got != "parent" {
		t.Fatalf("parent branch = %q", got)
	}
	bundle.ParentSessionID = "parent-id"
	if got := firstBranch(bundle); got != "child" {
		t.Fatalf("child branch = %q", got)
	}
}

func TestFitEvalExportSkipsIndivisibleFloorAndFitsOtherText(t *testing.T) {
	prompts := []EvalPrompt{{Text: strings.Repeat("a", 255) + "界"}, {Text: strings.Repeat("b", 257)}}
	got := FitEvalExport(EvalExport{Prompts: &prompts}, 1)
	if (*got.Prompts)[0].Text != prompts[0].Text || len((*got.Prompts)[1].Text) != 256 || got.Trimmed.TextsTruncated != 1 || !got.Trimmed.ExceedsMaxBytes {
		t.Fatalf("prompts %+v trimmed %+v", *got.Prompts, got.Trimmed)
	}
}
