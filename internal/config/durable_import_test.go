package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDurableImportFenceComposesAndSurvivesRollback(t *testing.T) {
	protected, _ := blanketConfig(t)
	for _, cfg := range []Config{{MachineID: "synthetic"}, protected} {
		cfg.DurableImportProtection = true
		cfg.GenerationProtection = true
		home := t.TempDir()
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, found, err := Load(home)
		if err != nil || !found || loaded.SchemaVersion != 5 || !loaded.DurableImportProtection || !loaded.GenerationProtection {
			t.Fatal(loaded, err)
		}
		raw, err := json.Marshal(loaded)
		if err != nil || !bytes.Contains(raw, []byte(`"writer":"staged-imports-v5"`)) {
			t.Fatal(string(raw), err)
		}
		var legacy struct {
			SchemaVersion int `json:"schema_version"`
		}
		if json.Unmarshal(raw, &legacy) == nil {
			t.Fatal("numeric old writer accepted staged imports")
		}
		if err = Save(home, Config{MachineID: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		rolled, _, err := Load(home)
		if err != nil || !rolled.DurableImportProtection || rolled.SchemaVersion != 5 {
			t.Fatal(rolled, err)
		}
	}
	legacy, err := json.Marshal(Config{MachineID: "synthetic"})
	if err != nil || bytes.Contains(legacy, []byte(`"writer"`)) {
		t.Fatal(string(legacy), err)
	}
}
