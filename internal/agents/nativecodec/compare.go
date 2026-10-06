package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"strings"
	"time"
)

// EvidenceExtends interprets retained native record compatibility only.
// Version transitions and rewrite decisions belong to shared orchestration.
func EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	format := previous.Capture.SourceFormat
	previousRecords := WithoutSubagentMeta(format, previous.NativeRecords)
	candidateRecords := WithoutSubagentMeta(format, candidate.NativeRecords)
	if len(candidateRecords) < len(previousRecords) || len(candidate.NativeText) < len(previous.NativeText) {
		return false
	}
	for i := range previousRecords {
		if !SameNativeRecord(format, previousRecords[i], candidateRecords[i]) {
			return false
		}
	}
	for i := range previous.NativeText {
		if previous.NativeText[i].Format != candidate.NativeText[i].Format || !strings.HasPrefix(candidate.NativeText[i].Content, previous.NativeText[i].Content) {
			return false
		}
	}
	return true
}

// ClaudeNamingOnlyChange compares all retained evidence except typed native
// titles and their capture bookkeeping. Source identity and supplemental facts
// must remain identical; a conversation edit always advances activity.
func ClaudeNamingOnlyChange(previous, candidate archive.SourceBundle) bool {
	if previous.Capture.SourceFormat != "claude-jsonl" || candidate.Capture.SourceFormat != "claude-jsonl" {
		return false
	}
	strip := func(b archive.SourceBundle) archive.SourceBundle {
		records := make([]map[string]any, 0, len(b.NativeRecords))
		for _, record := range b.NativeRecords {
			if !isClaudeTitleRecord(record) {
				records = append(records, record)
			}
		}
		b.NativeRecords = records
		if len(b.NativeText) == 0 {
			b.NativeText = nil
		}
		if len(b.SupplementalEvidence) == 0 {
			b.SupplementalEvidence = nil
		}
		if len(b.LinkedSessions) == 0 {
			b.LinkedSessions = nil
		}
		b.Capture.CapturedAt = time.Time{}
		b.Capture.Boundary = archive.CaptureBoundary{}
		b.Capture.Gaps = nil
		return b
	}
	return reflect.DeepEqual(strip(previous), strip(candidate))
}
