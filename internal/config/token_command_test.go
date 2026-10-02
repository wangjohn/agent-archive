package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenCommandConfigValidatesArgvAndPreservesOlderConfigs(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{""}, {"program", "bad\nargument"}, {"program", strings.Repeat("x", 2049)}, make([]string, 65)} {
		if err := (Config{CloudflareTokenCommand: args}).ValidateCloudflareTokenCommand(); err == nil {
			t.Fatal("unsafe command accepted")
		}
	}
	home := t.TempDir()
	cfg := Config{CloudflareTokenCommand: []string{"op", "read", "op://Private/Cloudflare/agent-archive"}}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(home)
	if err != nil || !found || !reflect.DeepEqual(got.CloudflareTokenCommand, cfg.CloudflareTokenCommand) {
		t.Fatalf("argv not preserved %v", err)
	}
	if err := (Config{}).ValidateCloudflareTokenCommand(); err != nil {
		t.Fatal("older config rejected")
	}
}
