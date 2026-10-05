package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestSourceMetadataPreservesSymlinkParentPath(t *testing.T) {
	root := t.TempDir()
	spelled := filepath.Join(root, "spelled")
	resolved := filepath.Join(root, "resolved")
	for _, dir := range []string{spelled, filepath.Join(resolved, "subdir")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(spelled, "link")
	if err := os.Symlink(filepath.Join(resolved, "subdir"), link); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(resolved, "agent-child.jsonl"):     `{"type":"user","message":{"role":"user","content":"Synthetic prompt"}}` + "\n",
		filepath.Join(resolved, "agent-child.meta.json"): `{"description":"Correct sibling"}`,
		filepath.Join(spelled, "agent-child.meta.json"):  `{"description":"Wrong lexical sibling"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: transcriptio.OS{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Join would erase the symlink/.. traversal that the filesystem resolves.
	path := link + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "agent-child.jsonl"
	snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{SubagentMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := (Filter{}).Filter(t.Context(), snapshot.Input(), agentapi.FilterContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) == 0 {
		t.Fatal("missing filtered child records")
	}
	var lead struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(filtered.Records[0], &lead); err != nil {
		t.Fatal(err)
	}
	if lead.Type != "subagent-meta" || lead.Description != "Correct sibling" {
		t.Fatalf("metadata from wrong directory: %+v", lead)
	}
}

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
