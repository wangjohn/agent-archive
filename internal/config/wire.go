package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// configJSON prevents recursive calls through Config's JSON methods.
type configJSON Config

type configWriter string

const (
	legacyDiscoveryWriter configWriter = "discovery-v2"
	discoveryWriter       configWriter = "discovery-floor-v2"
)

const legacyCodexWriter configWriter = "codex-scope-v3"

const codexWriter configWriter = "codex-scope-floor-v3"

type writerVersion struct {
	Version int          `json:"version"`
	Writer  configWriter `json:"writer"`
}

// MarshalJSON keeps legacy configuration numeric and fences protected writers
// through the known schema_version field. Published older integer decoders
// reject this object before they can discard unknown authorization fields.
// Normalize a value copy so edited nested setup drafts retain that fence.
func (c Config) MarshalJSON() ([]byte, error) {
	if err := prepareDiscoveryConfig(&c); err != nil {
		return nil, err
	}
	plain := configJSON(c)
	if c.Discovery == nil && c.CodexCapture == nil {
		return json.Marshal(plain)
	}
	if err := validateDiscoveryConfig(c); err != nil {
		return nil, err
	}
	version := writerVersion{Version: 2, Writer: discoveryWriter}
	if c.CodexCapture != nil {
		version = writerVersion{Version: 3, Writer: codexWriter}
	}
	return json.Marshal(struct {
		SchemaVersion writerVersion `json:"schema_version"`
		*configJSON
	}{SchemaVersion: version, configJSON: &plain})
}

// UnmarshalJSON reads legacy numeric versions and the protected writer fence.
// Numeric discovery configs remain readable for guarded canonical migration.
func (c *Config) UnmarshalJSON(data []byte) error {
	_, err := decodeConfig(data, c)
	return err
}

func decodeConfig(data []byte, c *Config) (bool, error) {
	plain := configJSON(*c)
	wire := struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		*configJSON
	}{configJSON: &plain}
	if err := json.Unmarshal(data, &wire); err != nil {
		return false, err
	}
	raw := bytes.TrimSpace(wire.SchemaVersion)
	fenced := len(raw) > 0 && raw[0] == '{'
	if fenced {
		var version writerVersion
		if err := json.Unmarshal(raw, &version); err != nil {
			return false, err
		}
		if (version.Version != 2 || (version.Writer != discoveryWriter && version.Writer != legacyDiscoveryWriter)) && (version.Version != 3 || (version.Writer != codexWriter && version.Writer != legacyCodexWriter)) {
			return false, errors.New("configuration requires a supported writer fence")
		}
		if version.Version == 3 {
			if plain.CodexCapture == nil || (!strings.HasSuffix(string(plain.SkillEvidence), codexWriterMarker) && (version.Writer != legacyCodexWriter || !strings.HasSuffix(string(plain.SkillEvidence), legacyCodexWriterMarker))) {
				return false, errors.New("writer fence requires Codex policy")
			}
		} else if plain.Discovery == nil || !strings.HasSuffix(string(plain.SkillEvidence), discoveryWriterMarker) {
			return false, errors.New("writer fence requires protected discovery configuration")
		}
		if version.Writer == discoveryWriter {
			for _, a := range plain.Discovery.Authorizations {
				if len(a.Intervals) > 0 && a.NativeStartFloor.IsZero() {
					return false, errors.New("discovery permission history requires its native start floor")
				}
			}
		}
		if version.Writer == codexWriter {
			var scopes []*DiscoveryAuthorization
			scopes = append(scopes, plain.CodexCapture.Authorization, plain.CodexCapture.SourceAuthorization)
			if plain.Discovery != nil {
				for i := range plain.Discovery.Authorizations {
					scopes = append(scopes, &plain.Discovery.Authorizations[i])
				}
			}
			for _, a := range scopes {
				if a != nil && len(a.Intervals) > 0 && a.NativeStartFloor.IsZero() {
					return false, errors.New("codex permission history requires its native start floor")
				}
			}
		}
		// Prior protected writers also need canonical migration before identity
		// mutation: they do not understand immutable generation floors.
		fenced = version.Writer == discoveryWriter || version.Writer == codexWriter
		plain.SchemaVersion = version.Version
	} else if len(raw) > 0 {
		if err := json.Unmarshal(raw, &plain.SchemaVersion); err != nil {
			return false, err
		}
	}
	*c = Config(plain)
	return fenced, nil
}
