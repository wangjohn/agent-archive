package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProtectedConfigurationRejectsPublishedIntegerVersionReader(t *testing.T) {
	t.Parallel()
	cfg, _ := discoveryConfig(t)
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Published v0.1.1 commit48dbafec config.Load uses plain json.Unmarshal
	// with this known integer field, no version check or SkillEvidence enum.
	// This pins its decoder contract, not actual Darwin binary acceptance.
	var published struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &published); err == nil {
		t.Fatal("published integer decoder accepted protected authorization")
	}
	loaded, found, err := Load(home)
	if err != nil || !found || !reflect.DeepEqual(loaded, cfg) {
		t.Fatalf("protected roundtrip changed config: %#v %v", loaded, err)
	}
}

func TestNumericProtectedConfigurationMigratesBeforeIdentityWriting(t *testing.T) {
	t.Parallel()
	cfg, _ := discoveryConfig(t)
	home := t.TempDir()
	// configJSON models the numeric schema2 format from before the fence.
	raw, err := json.Marshal(configJSON(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProtectIdentityWriter(home); err != nil {
		t.Fatal(err)
	}
	loaded, found, fenced, err := loadConfig(home)
	if err != nil || !found || !fenced || !reflect.DeepEqual(loaded, cfg) {
		t.Fatalf("identity guard lost consent or failed to fence: %#v %t %v", loaded, fenced, err)
	}
	before, err := os.Stat(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ProtectIdentityWriter(home); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(home, "config.json"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("already fenced config was unnecessarily rewritten", err)
	}
}

func TestUnknownOrMalformedWriterFenceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, version := range []string{
		`{"version":3,"writer":"discovery-v2"}`,
		`{"version":2,"writer":"unknown"}`,
		`{"version":"2","writer":"discovery-v2"}`,
		`{"version":2}`,
		`[]`,
		`"discovery-v2"`,
	} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			cfg, _ := discoveryConfig(t)
			raw, err := json.Marshal(configJSON(cfg))
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			document["schema_version"] = json.RawMessage(version)
			raw, err = json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load(home); err == nil {
				t.Fatal("unsupported writer fence accepted")
			}
			if err := ProtectIdentityWriter(home); err == nil {
				t.Fatal("identity guard rewrote unsupported writer fence")
			}
		})
	}
}

func TestProtectedWireEncodingCannotDowngradeFutureSchema(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		cfg, _ := discoveryConfig(t)
		cfg.Discovery.Enabled = enabled
		cfg.SchemaVersion = 3
		cfg.SkillEvidence = SkillEvidenceNone
		if _, err := json.Marshal(cfg); err == nil {
			t.Fatal("protected encoding downgraded a newer schema")
		}
		if err := Save(t.TempDir(), cfg); err == nil {
			t.Fatal("protected save downgraded a newer schema")
		}
		if cfg.SchemaVersion != 3 || cfg.SkillEvidence != SkillEvidenceNone {
			t.Fatal("failed wire encoding changed caller")
		}
	}
}
