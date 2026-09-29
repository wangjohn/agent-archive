package archive

import (
	"errors"
	"time"
)

// MaxSubagentTypeLength is the longest subagent type SanitizeSubagentType
// keeps.
const MaxSubagentTypeLength = 64

// SanitizeSubagentType returns the subagent type a SubagentStop hook reported
// ("Explore", "general-purpose", "my-plugin:reviewer") when it is at most
// MaxSubagentTypeLength characters, all ASCII letters, digits, or one of
// "_.:-", and "" otherwise. The type is harness-provided text that may reach
// a capture gap's detail, so anything else is dropped whole rather than
// trimmed. It is informational only and never drives a decision.
func SanitizeSubagentType(agentType string) string {
	if agentType == "" || len(agentType) > MaxSubagentTypeLength {
		return ""
	}
	for i := range len(agentType) {
		c := agentType[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == ':' || c == '-':
		default:
			return ""
		}
	}
	return agentType
}

// NewCaptureGapEvidence builds capture_gap supplemental evidence with code
// and detail, filtered as FilterSupplementalEvidence filters every producer's
// evidence, so a detail holding a credential is redacted before it is saved.
func NewCaptureGapEvidence(code, detail, provenance string, observedAt time.Time) (SupplementalEvidence, error) {
	if code == "" {
		return SupplementalEvidence{}, errors.New("capture gap evidence requires a code")
	}
	payload := map[string]any{"code": code}
	if detail != "" {
		payload["detail"] = detail
	}
	filtered, _, err := FilterSupplementalEvidence([]SupplementalEvidence{{
		Kind: EvidenceKindCaptureGap, ObservedAt: observedAt, Provenance: provenance, Payload: payload,
	}})
	if err != nil {
		return SupplementalEvidence{}, err
	}
	if len(filtered) != 1 {
		return SupplementalEvidence{}, errors.New("capture gap evidence was not retained")
	}
	return filtered[0], nil
}
