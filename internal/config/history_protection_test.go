package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestHistoryWriterFenceComposesAndStaysSticky(t *testing.T) {
	protected, _ := blanketConfig(t)
	for _, cfg := range []Config{{MachineID: "synthetic"}, protected} {
		cfg.HistoryProtection = true
		cfg.DurableImportProtection = true
		cfg.GenerationProtection = true
		home := t.TempDir()
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		got, found, err := Load(home)
		if err != nil || !found || got.SchemaVersion != 6 || !got.HistoryProtection || !got.DurableImportProtection || !got.GenerationProtection {
			t.Fatal(got, err)
		}
		raw, err := json.Marshal(got)
		if err != nil || !bytes.Contains(raw, []byte(`"writer":"history-lifecycle-v6"`)) {
			t.Fatal(string(raw), err)
		}
		if err = Save(home, Config{MachineID: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		rolled, _, err := Load(home)
		if err != nil || !rolled.HistoryProtection || rolled.SchemaVersion != 6 {
			t.Fatal(rolled, err)
		}
	}
}

func TestHistoryWriterRejectsMarkerWithoutCapability(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"schema_version":{"version":6,"writer":"history-lifecycle-v6"}}`), &cfg); err == nil {
		t.Fatal("history marker accepted without protection")
	}
}
