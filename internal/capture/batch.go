package capture

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"time"
	"unicode/utf8"
)

const maxLifecycleEvents = 64
const maxLifecycleBytes = 16 << 20

// validateBatch checks every decoded fact and filters every evidence candidate
// before configuration diagnostics, queued intent or archive writes can occur.
func validateBatch(harness string, in []agentapi.LifecycleEvent, now time.Time) ([]agentapi.LifecycleEvent, error) {
	if len(in) > 0 && now.IsZero() {
		return nil, errors.New("lifecycle observation time required")
	}
	if len(in) > maxLifecycleEvents {
		return nil, errors.New("lifecycle batch count exceeded")
	}
	out := make([]agentapi.LifecycleEvent, len(in))
	copy(out, in)
	var identity agentmeta.SessionKey
	budget := maxLifecycleBytes
	for i, event := range out {
		key, err := agentmeta.NewSessionKey(string(event.Session.Agent), event.Session.NativeID)
		if err != nil {
			return nil, fmt.Errorf("invalid lifecycle identity: %w", err)
		}
		if string(key.Agent) != archive.CanonicalHarness(harness) || key.Agent != event.Session.Agent {
			return nil, errors.New("lifecycle agent mismatch")
		}
		if i == 0 {
			identity = key
		} else if key != identity || event.ProjectRoot != out[0].ProjectRoot {
			return nil, errors.New("inconsistent lifecycle batch identity")
		}
		if err := validateEventShape(event); err != nil {
			return nil, err
		}
		if err := validateEventFields(event, &budget); err != nil {
			return nil, err
		}
		retained, err := validateEventEvidence(event, now, &budget)
		if err != nil {
			return nil, err
		}
		out[i].Evidence = retained
	}
	return out, nil
}
func validateEventShape(event agentapi.LifecycleEvent) error {
	if event.Kind < agentapi.EventStart || event.Kind > agentapi.EventSubagent {
		return errors.New("invalid lifecycle kind")
	}
	if event.Start.Kind > agentapi.FreshStat || event.Locator > agentapi.LocatorFillFile || event.Deferred > agentapi.DeferredFollowup {
		return errors.New("invalid lifecycle field")
	}
	if event.Kind != agentapi.EventStart && event.Start.Kind != agentapi.FreshUnknown {
		return errors.New("freshness on non-start lifecycle effect")
	}
	if event.NewOnly && event.Kind != agentapi.EventStart {
		return errors.New("new-only policy on non-start effect")
	}
	if event.Deferred == agentapi.DeferredStart && event.Kind != agentapi.EventStart || event.Deferred == agentapi.DeferredFollowup && event.Kind != agentapi.EventStop && event.Kind != agentapi.EventResponse {
		return errors.New("invalid deferred lifecycle effect")
	}
	if event.Kind == agentapi.EventSubagent && event.Child == nil || event.Kind != agentapi.EventSubagent && event.Child != nil {
		return errors.New("invalid child lifecycle observation")
	}
	return validateEventValues(event)
}
func validateEventValues(event agentapi.LifecycleEvent) error {
	if event.Source.Kind != "" && event.Source.Kind != archive.SourceKindFile {
		return errors.New("hook locator must name a file source")
	}
	if event.Source.Key != "" {
		return errors.New("file hook locator cannot carry a database key")
	}
	if event.Start.Kind != agentapi.FreshStat && event.Start.Path != "" {
		return errors.New("stat path without stat request")
	}
	if len(event.Reason) > 128 || strings.TrimSpace(event.Reason) == "" || strings.ContainsAny(event.Reason, "\x00\r\n") {
		return errors.New("invalid lifecycle reason")
	}
	if event.Kind == agentapi.EventStart && !validFreshnessReason(event.Start.Reason) {
		return errors.New("invalid freshness reason code")
	}
	if len(event.Start.Reason) > 128 {
		return errors.New("invalid freshness reason")
	}
	return nil
}
func validateEventFields(event agentapi.LifecycleEvent, budget *int) error {
	fields := []string{string(event.Session.Agent), event.Session.NativeID, event.Session.Version, event.Session.Mode, event.ProjectRoot, event.Source.Path, event.Source.Key, event.Start.Path, event.Start.Reason, event.Reason, event.NativeEvent}
	if event.Child != nil {
		fields = append(fields, event.Child.ID, event.Child.Path, event.Child.Type, event.Child.MissingDetail)
	}
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return errors.New("invalid lifecycle UTF-8")
		}
		*budget -= len(field)
		if *budget < 0 {
			return errors.New("lifecycle batch byte limit exceeded")
		}
	}
	return nil
}
func validateEventEvidence(event agentapi.LifecycleEvent, now time.Time, budget *int) ([]archive.SupplementalEvidence, error) {
	if len(event.Evidence) > 16 {
		return nil, errors.New("lifecycle evidence count exceeded")
	}
	var retained []archive.SupplementalEvidence
	for _, candidate := range event.Evidence {
		if candidate.Kind != archive.EvidenceKindLifecycleHook && candidate.Kind != archive.EvidenceKindFinalResponse {
			return nil, errors.New("invalid lifecycle evidence kind")
		}
		if !candidate.ObservedAt.Equal(now) {
			return nil, errors.New("inconsistent lifecycle observation time")
		}
		if !utf8.ValidString(candidate.Provenance) || strings.ContainsAny(candidate.Provenance, "\x00\r\n") {
			return nil, errors.New("invalid lifecycle provenance")
		}
		*budget -= len(candidate.Provenance)
		if !boundedEvidence(candidate.Payload, budget, 0) {
			return nil, errors.New("lifecycle evidence limit exceeded")
		}
		filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{candidate})
		if err != nil {
			return nil, err
		}
		for _, safe := range filtered {
			archive.AnnotateSupplementalGaps(safe.Payload, gaps)
			retained = append(retained, safe)
		}
	}
	return retained, nil
}
func boundedEvidence(value any, budget *int, depth int) bool {
	if depth > 32 || *budget < 0 {
		return false
	}
	switch v := value.(type) {
	case nil, bool, float64, int, int64:
		*budget -= 8
	case string:
		*budget -= len(v)
		if !utf8.ValidString(v) {
			return false
		}
	case []any:
		if len(v) > maxLifecycleBytes {
			return false
		}
		for _, item := range v {
			if !boundedEvidence(item, budget, depth+1) {
				return false
			}
		}
	case map[string]any:
		if len(v) > maxLifecycleBytes {
			return false
		}
		for key, item := range v {
			*budget -= len(key)
			if !utf8.ValidString(key) || !boundedEvidence(item, budget, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	return *budget >= 0
}

func validFreshnessReason(reason string) bool {
	switch reason {
	case "empty_native_path", "invalid_native_path", "inspect_native_path", "explicit_start", "explicit_continuation", "unknown_native_source", "retained_hook_proof":
		return true
	}
	return false
}
