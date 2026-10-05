package discovery

import (
	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

const maxObservedFormats = 16

// FormatObservation summarizes compatible session metadata seen in a scan.
// Counts are observations, not unique sessions or verified publications.
type FormatObservation struct {
	Profile      sourcefacts.CodexProfile  `json:"profile"`
	Version      string                    `json:"producer_version"`
	Source       string                    `json:"producer_source"`
	Evidence     sourcefacts.CodexEvidence `json:"evidence"`
	Observations int                       `json:"observations"`
}

func (h *Health) observeFormat(c Candidate) {
	source, _ := json.Marshal(c.ProducerSource)
	f := FormatObservation{Profile: c.FormatProfile, Version: c.HarnessVersion, Source: c.ProducerSource,
		Evidence: sourcefacts.CodexProducerEvidence(sourcefacts.CodexMeta{Source: source, Version: c.HarnessVersion, Originator: c.ProducerOriginator}), Observations: 1}
	if !validFormatObservation(f) {
		return
	}
	for i := range h.Formats {
		prior := h.Formats[i]
		prior.Observations = 1
		if prior == f {
			h.Formats[i].Observations++
			return
		}
	}
	if len(h.Formats) == maxObservedFormats {
		h.Outcomes["format_summary_overflow"]++
		return
	}
	h.Formats = append(h.Formats, f)
}

func validFormatObservation(f FormatObservation) bool {
	return (f.Profile == sourcefacts.CodexLegacyJSONL || f.Profile == sourcefacts.CodexPaginatedJSONL) &&
		nativesessions.ValidCodexVersion(f.Version) &&
		(f.Source == "cli" || f.Source == "exec" || f.Source == "vscode") &&
		(f.Evidence == sourcefacts.CodexRuntimeTested || f.Evidence == sourcefacts.CodexSourceInspected || f.Evidence == sourcefacts.CodexCompatibleUntested) &&
		f.Observations > 0
}
