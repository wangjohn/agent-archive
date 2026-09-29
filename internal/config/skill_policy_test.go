package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
	updated.SkillEvidence = "invalid"
	if err := Save(home, updated); err == nil {
		t.Fatal("accepted invalid mode")
	}
}
