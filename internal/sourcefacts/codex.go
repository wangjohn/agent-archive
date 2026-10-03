// Package sourcefacts resolves bounded native source facts and project ownership.
package sourcefacts

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"time"
)

// CodexMeta is the shared native identity/execution metadata shape.
type CodexMeta = nativesessions.CodexMeta

// ParseCodexMeta decodes shared native metadata without admission policy.
func ParseCodexMeta(line []byte) (CodexMeta, time.Time, bool, error) {
	return nativesessions.ParseCodexMeta(line)
}

// RolloutID returns a rollout filename identity.
func RolloutID(path string) string { return nativesessions.RolloutID(path) }

// NativeFirstTask classifies the first native task metadata event.
func NativeFirstTask(line []byte) (bool, bool) { return nativesessions.NativeFirstTask(line) }

// FirstTaskAt reads native start time without retaining body.
func FirstTaskAt(line []byte) time.Time { return nativesessions.FirstTaskAt(line) }

func present(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }

// SupportedCodexProducer remains disabled until the activation PR establishes
// supported format/version combinations under the approved source contract.
func SupportedCodexProducer(CodexMeta) bool { return false }
