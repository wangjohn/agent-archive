package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestCatalogFormatDefaultsAndWriterFence(t *testing.T) {
	legacy := Config{SchemaVersion: 1}
	if legacy.Storage.EffectiveArchiveFormat() != destination.FormatLegacy {
		t.Fatal("legacy default changed")
	}
	legacy.Storage.ArchiveFormat = destination.FormatCatalogV4
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"writer":"catalog-v4-v8"`) {
		t.Fatal("missing catalog old-writer fence")
	}
	var decoded Config
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 8 || decoded.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		t.Fatal("format roundtrip")
	}
	var old struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err = json.Unmarshal(raw, &old); err == nil {
		t.Fatal("old integer writer admitted catalog")
	}
	const unknownFormat destination.ArchiveFormat = "unknown"
	legacy.Storage.ArchiveFormat = unknownFormat
	if _, err = json.Marshal(legacy); err == nil {
		t.Fatal("unknown format admitted")
	}
}
