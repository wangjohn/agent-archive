package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
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
