package nativesessions

import (
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"time"
)

// CodexMeta is shared bounded native metadata, independent of capture policy.
type CodexMeta = codexmeta.CodexMeta

// Codex history representations understood by native discovery.
const (
	CodexHistoryLegacy    = codexmeta.CodexHistoryLegacy
	CodexHistoryPaginated = codexmeta.CodexHistoryPaginated
)

// RolloutID returns the physical history segment ID from a native filename.
func RolloutID(path string) string { return codexmeta.RolloutID(path) }

// ParseCodexMeta decodes a native metadata record without capture policy.
func ParseCodexMeta(line []byte) (CodexMeta, time.Time, bool, error) {
	return codexmeta.ParseCodexMeta(line)
}

// ValidCodexVersion bounds the diagnostic producer token.
func ValidCodexVersion(v string) bool { return codexmeta.ValidCodexVersion(v) }

// ValidCodexExecutionSource recognizes local execution tags without proving provenance.
func ValidCodexExecutionSource(v string) bool { return codexmeta.ValidCodexExecutionSource(v) }

// NativeFirstTask validates the first native task's identity and timestamp.
func NativeFirstTask(line []byte) (bool, bool) { return codexmeta.NativeFirstTask(line) }

// FirstTaskAt returns the task's recorded start time.
func FirstTaskAt(line []byte) time.Time { return codexmeta.FirstTaskAt(line) }
