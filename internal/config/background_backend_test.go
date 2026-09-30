package config

import (
	"bytes"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/local"
)

// The scheduler backend is one optional field: absent when none is recorded
// (launchd, and every configuration from before the field), and read back as
// recorded.
func TestBackgroundBackendRoundTripsAndOmitsWhenEmpty(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Config{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path(home))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("background_backend")) {
		t.Fatalf("an empty background_backend was written:\n%s", data)
	}
	if err := Save(home, Config{BackgroundBackend: "systemd"}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"background_backend": "systemd"`)) {
		t.Fatalf("the backend was not written as background_backend:\n%s", data)
	}
	if cfg, _, err := Load(home); err != nil || cfg.BackgroundBackend != "systemd" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}

// A release from before the field reads a configuration that has it (configs
// and journals are read with plain json.Unmarshal, which ignores what it does
// not know), and what it reads is what it always read. The earlier release is
// the struct it decoded into, written out here.
func TestAReleaseBeforeBackgroundBackendIgnoresIt(t *testing.T) {
	home := t.TempDir()
	want := Config{SchemaVersion: SchemaVersion, MachineID: "m-1", Paused: true, RetentionDays: 90, BackgroundBackend: "systemd"}
	if err := Save(home, want); err != nil {
		t.Fatal(err)
	}
	var earlier struct {
		SchemaVersion int    `json:"schema_version"`
		MachineID     string `json:"machine_id"`
		Paused        bool   `json:"paused"`
		RetentionDays int    `json:"retention_days"`
	}
	if err := local.Read(path(home), &earlier); err != nil {
		t.Fatalf("an earlier release cannot read the configuration: %v", err)
	}
	if earlier.SchemaVersion != SchemaVersion || earlier.MachineID != "m-1" || !earlier.Paused || earlier.RetentionDays != 90 {
		t.Errorf("an earlier release read %+v", earlier)
	}
}
