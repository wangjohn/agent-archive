package config

import (
	"strings"
	"testing"
)

func TestCodexNamingModeIsOptionalAndValidated(t *testing.T) {
	unsupportedCodexNameLookup := CodexNameLookup(strings.ToLower("AUTOMATIC"))
	for _, mode := range []CodexNameLookup{"", CodexNameLookupFiles, CodexNameLookupNative} {
		if err := (Config{CodexNameLookup: mode}).ValidateCodexNameLookup(); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Config{CodexNameLookup: unsupportedCodexNameLookup}).ValidateCodexNameLookup(); err == nil {
		t.Fatal("unknown mode accepted")
	}
	home := t.TempDir()
	cfg := Config{CodexNameLookup: CodexNameLookupNative}
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := Load(home)
	if err != nil || !ok || loaded.CodexNameLookup != CodexNameLookupNative {
		t.Fatal(loaded, ok, err)
	}
	cfg.CodexNameLookup = unsupportedCodexNameLookup
	if err := Save(home, cfg); err == nil {
		t.Fatal("invalid mode saved")
	}
}
