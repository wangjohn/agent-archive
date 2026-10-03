package claude

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestMetadataFailedOpenRetainsCleanupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-child.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "agent-child.meta.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("synthetic failed-open cleanup fault")
	files := metadataFailedOpenFiles{fault: fault}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{SubagentMetadata: true})
	if !errors.Is(err, fault) || !agentapi.HasFailure(err, agentapi.Cleanup) {
		t.Fatalf("metadata open hid cleanup: %v", err)
	}
}

type metadataFailedOpenFiles struct {
	transcriptio.OS
	fault error
}

func (f metadataFailedOpenFiles) OpenRegular(path string) (transcriptio.File, error) {
	if filepath.Ext(path) == ".json" {
		return nil, errors.Join(os.ErrPermission, transcriptio.ErrCleanup, f.fault)
	}
	return f.OS.OpenRegular(path)
}
