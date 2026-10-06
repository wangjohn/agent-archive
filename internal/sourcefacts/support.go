package sourcefacts

import (
	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

// CodexProfile identifies an understood on-disk history representation. It is
// independent of release numbers and never grants capture consent.
type CodexProfile string

// Supported Codex JSONL history profiles.
const (
	CodexLegacyJSONL    CodexProfile = "codex_jsonl_legacy"
	CodexPaginatedJSONL CodexProfile = "codex_jsonl_paginated"
)

// CodexFormatProfile checks metadata only. Admission also requires bounded
// ReadCodexHeader first-task validation, identity and original-creation consent.
// Unknown additive fields are tolerated; unknown history/source forms are not.
func CodexFormatProfile(m CodexMeta) CodexProfile {
	if m.FormatOutcome() != codexmeta.NativeFormat {
		return ""
	}
	switch m.HistoryMode {
	case "", nativesessions.CodexHistoryLegacy:
		return CodexLegacyJSONL
	case nativesessions.CodexHistoryPaginated:
		return CodexPaginatedJSONL
	default:
		return ""
	}
}

// SupportedCodexProducer reports metadata format compatibility, not a release
// allowlist or proof of originating execution. Full admission requires a native
// first task and policy checks even for previously tested producer versions.
func SupportedCodexProducer(m CodexMeta) bool { return CodexFormatProfile(m) != "" }

// CodexEvidence describes evidence behind a compatible observation. These
// labels are diagnostic only; untested compatible producers remain eligible.
type CodexEvidence string

// Evidence levels distinguish exercised producers from structural compatibility.
const (
	CodexRuntimeTested      CodexEvidence = "runtime_tested"
	CodexSourceInspected    CodexEvidence = "source_inspected"
	CodexCompatibleUntested CodexEvidence = "compatible_untested"
)

type codexProducerVersion string

const (
	codexVersion150           codexProducerVersion = "0.150.0"
	codexVersion155           codexProducerVersion = "0.155.0"
	codexVersion155Alpha9     codexProducerVersion = "0.155.0-alpha.9"
	codexVersion155Alpha9Dot2 codexProducerVersion = "0.155.0-alpha.9.2"
	codexVersion159           codexProducerVersion = "0.159.3"
	codexVersion160           codexProducerVersion = "0.160.0"
)

// CodexProducerEvidence describes pinned evidence for observed metadata, never
// the executable on PATH. Runtime evidence includes a synthetic app-server
// client named Codex Desktop, not acceptance of the desktop GUI.
func CodexProducerEvidence(m CodexMeta) CodexEvidence {
	var source string
	_ = json.Unmarshal(m.Source, &source)
	known := (source == "cli" && m.Originator == "codex-tui") ||
		(source == "exec" && m.Originator == "codex_exec") ||
		(source == "vscode" && m.Originator == "Codex Desktop")
	if known {
		switch codexProducerVersion(m.Version) {
		case codexVersion159:
			return CodexRuntimeTested
		case codexVersion150, codexVersion155, codexVersion155Alpha9, codexVersion155Alpha9Dot2, codexVersion160:
			return CodexSourceInspected
		}
	}
	return CodexCompatibleUntested
}
