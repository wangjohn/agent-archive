package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SessionLabel is a bounded filtered native observation. Its zero value is unavailable.
type SessionLabel struct {
	NativeID string `json:"native_session_id"`
	State    string `json:"state"`
	Name     string `json:"name,omitempty"`
	Source   string `json:"source"`
	Contract string `json:"contract"`
}

// SessionLabelContract identifies the verified Codex file resolution semantics.
const SessionLabelContract = "codex-files-159.2-v1"

// FilterSessionLabel validates the narrow shape and filters before any persistence.
func FilterSessionLabel(label SessionLabel) (SessionLabel, bool) {
	if label.Contract != SessionLabelContract || (label.Source != "index" && label.Source != "database") || (label.State != "present" && label.State != "confirmed_absent") || len(label.NativeID) != 36 {
		return SessionLabel{}, false
	}
	for i, c := range label.NativeID {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return SessionLabel{}, false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return SessionLabel{}, false
		}
	}
	if label.State == "confirmed_absent" {
		return label, label.Name == ""
	}
	if len(label.Name) > 16384 || !utf8.ValidString(label.Name) {
		return SessionLabel{}, false
	}
	state := PrivacyState{AddGap: func(string, int, string) {}}
	v, keep := sanitizeValue(label.Name, &state)
	name, ok := v.(string)
	if !keep || !ok {
		return SessionLabel{}, false
	}
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return SessionLabel{}, false
	}
	if len(name) > 512 {
		name = name[:512]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	label.Name = name
	return label, true
}

// Fingerprint hashes only filtered semantic content, never observation time.
func (label SessionLabel) Fingerprint() string {
	filtered, ok := FilterSessionLabel(label)
	if !ok {
		return ""
	}
	b, _ := json.Marshal(filtered)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Evidence retains the current native label without native paths or response data.
func (label SessionLabel) Evidence(at time.Time) SupplementalEvidence {
	payload := map[string]any{"native_session_id": label.NativeID, "state": label.State, "source": label.Source, "contract": label.Contract}
	if label.State == "present" {
		payload["name"] = label.Name
	}
	return SupplementalEvidence{Kind: EvidenceKindSessionLabels, ObservedAt: at, Provenance: "native:codex:session_labels", Payload: payload}
}

func labelFromEvidence(e SupplementalEvidence) (SessionLabel, bool) {
	if e.Kind != EvidenceKindSessionLabels || e.Provenance != "native:codex:session_labels" {
		return SessionLabel{}, false
	}
	for key := range e.Payload {
		if key != "native_session_id" && key != "state" && key != "name" && key != "source" && key != "contract" {
			return SessionLabel{}, false
		}
	}
	l := SessionLabel{}
	fields := []struct {
		key    string
		target *string
	}{{"native_session_id", &l.NativeID}, {"state", &l.State}, {"source", &l.Source}, {"contract", &l.Contract}}
	for _, field := range fields {
		value, ok := e.Payload[field.key].(string)
		if !ok {
			return SessionLabel{}, false
		}
		*field.target = value
	}
	if value, exists := e.Payload["name"]; exists {
		name, ok := value.(string)
		if !ok {
			return SessionLabel{}, false
		}
		l.Name = name
	}
	return FilterSessionLabel(l)
}

// CurrentSessionLabel returns only evidence matching its owning Codex bundle.
func CurrentSessionLabel(bundle SourceBundle) (SessionLabel, time.Time, bool) {
	if bundle.Capture.Harness.Name != "codex" {
		return SessionLabel{}, time.Time{}, false
	}
	for i := len(bundle.SupplementalEvidence) - 1; i >= 0; i-- {
		e := bundle.SupplementalEvidence[i]
		if l, ok := labelFromEvidence(e); ok && l.NativeID == bundle.NativeSessionID {
			return l, e.ObservedAt, true
		}
	}
	return SessionLabel{}, time.Time{}, false
}

// ValidateSessionLabels prevents a label from changing another source's name.
func ValidateSessionLabels(bundle SourceBundle) error {
	seen := false
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind != EvidenceKindSessionLabels {
			continue
		}
		l, ok := labelFromEvidence(e)
		if seen || !ok || !reflect.DeepEqual(l.Evidence(e.ObservedAt).Payload, e.Payload) || bundle.Capture.Harness.Name != "codex" || l.NativeID != bundle.NativeSessionID || bundle.History != nil || bundle.SchemaVersion != SourceSchemaVersion {
			return errors.New("session label does not match its owning ordinary Codex source")
		}
		seen = true
	}
	return nil
}
