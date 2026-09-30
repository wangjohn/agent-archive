package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The handoff block is edited by hand, so its spelling is part of the
// interface people type.
func TestHandoffConfigJSONSpellingIsPinned(t *testing.T) {
	cfg := Config{Handoff: HandoffConfig{
		Args:      map[string][]string{"codex": {"--model", "o3"}},
		DefaultTo: map[string]string{"claude": "codex"},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const want = `"handoff":{"args":{"codex":["--model","o3"]},"default_to":{"claude":"codex"}}`
	if !strings.Contains(string(data), want) {
		t.Fatalf("handoff JSON = %s, want it to contain %s", data, want)
	}
	data, err = json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "handoff") {
		t.Fatalf("an empty handoff block was written: %s", data)
	}
}

func TestHandoffConfigLoadsAndSaves(t *testing.T) {
	home := t.TempDir()
	body := `{"schema_version":1,"machine_id":"m","handoff":{"args":{"claude":["--model","opus"]},"default_to":{"cursor":"codex"}}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(home)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	want := HandoffConfig{Args: map[string][]string{"claude": {"--model", "opus"}}, DefaultTo: map[string]string{"cursor": "codex"}}
	if !reflect.DeepEqual(cfg.Handoff, want) {
		t.Fatalf("handoff = %+v", cfg.Handoff)
	}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	again, _, err := Load(home)
	if err != nil || !reflect.DeepEqual(again.Handoff, want) {
		t.Fatalf("round trip: %+v %v", again.Handoff, err)
	}
}

func TestHandoffConfigRejectsUnknownNames(t *testing.T) {
	for name, handoff := range map[string]string{
		"unknown args agent":     `{"args":{"gemini":["-x"]}}`,
		"empty argument":         `{"args":{"codex":[""]}}`,
		"NUL in argument":        `{"args":{"codex":["a\u0000b"]}}`,
		"unknown source harness": `{"default_to":{"gemini":"codex"}}`,
		"unknown destination":    `{"default_to":{"claude":"gemini"}}`,
	} {
		home := t.TempDir()
		body := `{"schema_version":1,"machine_id":"m","handoff":` + handoff + `}`
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(home); err == nil || !strings.Contains(err.Error(), "handoff") || !strings.Contains(err.Error(), "config.json") {
			t.Errorf("%s: Load err = %v", name, err)
		}
		var cfg Config
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := Save(t.TempDir(), cfg); err == nil {
			t.Errorf("%s: Save accepted it", name)
		}
	}
}
