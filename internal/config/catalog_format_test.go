package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

// Catalog configuration cannot borrow a legacy or missing writer fence, even
// while provider qualification still prevents production activation.
func TestLoadedCatalogRequiresItsForwardWriterFence(t *testing.T) {
	t.Parallel()
	for _, version := range []string{`1`, `7`, `8`, `9`, `10`, `null`, `{"version":9,"writer":"catalog-v4-v9"}`, `{"version":7,"writer":"durable-storage-v7"}`, `{"version":8,"writer":"durable-storage-v7"}`, `{"version":8}`, `{"version":8,"writer":"catalog-v4-v8"}`, `{"version":10}`, `{"version":10,"writer":"catalog-v4-v8"}`, `{"version":10,"writer":"catalog-v4-v10"}`} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			raw := []byte(`{"schema_version":` + version + `,"storage":{"ArchiveFormat":"catalog-v4"},"durable_storage_protection":true}`)
			if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, found, fenced, err := loadConfig(home)
			if version == `{"version":10,"writer":"catalog-v4-v10"}` {
				if err != nil || !found || !fenced {
					t.Fatal("valid catalog fence refused", err)
				}
			} else if err == nil || found {
				t.Fatal("catalog borrowed unsupported writer authority", version)
			}
		})
	}
	for _, raw := range []string{`{"storage":{"ArchiveFormat":"catalog-v4"}}`, `{"schema_version":1,"storage":{"ArchiveFormat":"catalog-v4"}}`} {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := Load(home); err == nil || found {
			t.Fatal("unfenced catalog configuration loaded")
		}
	}
}

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
	if !strings.Contains(string(raw), `"writer":"catalog-v4-v10"`) {
		t.Fatal("missing catalog old-writer fence")
	}
	var decoded Config
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 10 || decoded.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
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
