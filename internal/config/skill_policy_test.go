package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSkillsRoundTripsAndIsAbsentFromOlderConfigs(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"schema_version":1,"machine_id":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(home)
	if err != nil || !found || cfg.NoSkills {
		t.Fatalf("a config from before the field: %+v %v %v", cfg, found, err)
	}
	cfg.NoSkills = true
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || !strings.Contains(string(data), `"no_skills": true`) {
		t.Fatalf("saved config lacks no_skills: %s %v", data, err)
	}
	again, _, err := Load(home)
	if err != nil || !again.NoSkills {
		t.Fatalf("no_skills did not round-trip: %+v %v", again, err)
	}
	again.NoSkills = false
	if err := Save(home, again); err != nil {
		t.Fatal(err)
	}
	if data, err = os.ReadFile(filepath.Join(home, "config.json")); err != nil || strings.Contains(string(data), "no_skills") {
		t.Fatalf("a config with skills on writes no_skills: %s %v", data, err)
	}
}

func TestSkillEvidenceLegacyMigrationAndValidation(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"schema_version":1,"machine_id":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, found, err := Load(home)
	if err != nil || !found || legacy.EffectiveSkillEvidence() != SkillEvidenceBody {
		t.Fatalf("legacy: %+v %v %v", legacy, found, err)
	}
	legacy.SkillEvidence = SkillEvidenceMetadata
	if err := Save(home, legacy); err != nil {
		t.Fatal(err)
	}
	updated, _, err := Load(home)
	if err != nil || updated.SkillEvidence != SkillEvidenceMetadata || updated.SchemaVersion != 1 {
		t.Fatalf("updated: %+v %v", updated, err)
	}
	//lint:ignore LV1001 deliberately tests rejection of an unsupported policy value
	updated.SkillEvidence = "invalid"
	if err := Save(home, updated); err == nil {
		t.Fatal("accepted invalid mode")
	}
}
