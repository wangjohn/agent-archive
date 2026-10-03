package config

import (
	"bytes"
	"os"
	"testing"
)

// The recorded machine is one optional field: absent when none is recorded
// (macOS, and every configuration from before the field), and read back as
// recorded.
func TestHostIDRoundTripsAndOmitsWhenEmpty(t *testing.T) {
	home := t.TempDir()
	if err := Save(home, Config{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path(home))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("host_id")) {
		t.Fatalf("an empty host_id was written:\n%s", data)
	}
	if err := Save(home, Config{HostID: "abc123"}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"host_id": "abc123"`)) {
		t.Fatalf("the host ID was not written as host_id:\n%s", data)
	}
	if cfg, _, err := Load(home); err != nil || cfg.HostID != "abc123" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}
