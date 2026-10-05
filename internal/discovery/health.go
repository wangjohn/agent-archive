package discovery

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/local"
)

const maxHealthBytes = 16 * 1024

// healthSummary is separate from the source catalog; it never contains paths or IDs.
type healthSummary struct {
	Version int    `json:"version"`
	Health  Health `json:"health"`
}

func writeHealth(home string, h Health) error {
	if e := validateHealth(h); e != nil {
		return e
	}
	b, e := json.Marshal(healthSummary{Version: 1, Health: h})
	if e != nil {
		return e
	}
	if len(b) > maxHealthBytes {
		return errors.New("discovery health limit exceeded")
	}
	return local.Write(filepath.Join(home, "discovery-health.json"), healthSummary{Version: 1, Health: h})
}

func validateHealth(h Health) error {
	if h.Enabled && h.LastAttempt.IsZero() {
		return errors.New("discovery health has no attempt evidence")
	}
	if len(h.Errors) > 32 || len(h.Outcomes) > 64 || len(h.Formats) > maxObservedFormats {
		return errors.New("discovery health limits exceeded")
	}
	for _, f := range h.Formats {
		if !validFormatObservation(f) {
			return errors.New("invalid discovery format observation")
		}
	}
	for _, code := range h.Errors {
		if !validHealthError(code) {
			return errors.New("invalid discovery health diagnostic code")
		}
	}
	for code := range h.Outcomes {
		if len(code) > 64 {
			return errors.New("invalid discovery health outcome code")
		}
		for _, ch := range code {
			if ch != '_' && (ch < 'a' || ch > 'z') {
				return errors.New("invalid discovery health outcome code")
			}
		}
	}
	return nil
}

func validHealthError(code string) bool {
	allowed := []string{"catalog_rebuilt", "source_root_unavailable", "priority_source_unavailable", "retry_limit", "directory_limit", "removal_unavailable"}
	return slices.Contains(allowed, code)
}
