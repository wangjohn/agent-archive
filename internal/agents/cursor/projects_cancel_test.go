package cursor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/platform"
)

type cancelWorkspaceFiles struct {
	cancel context.CancelFunc
	reads  int
}

func (f *cancelWorkspaceFiles) ReadDir(path string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(path)
	f.cancel()
	return entries, err
}

func (f *cancelWorkspaceFiles) ReadFile(string) ([]byte, error) {
	f.reads++
	return []byte(`{"folder":"file:///synthetic/project"}`), nil
}

func TestWorkspaceCancellationStopsMetadataReads(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	storage := filepath.Join(home, ".config", "Cursor", "User", "workspaceStorage")
	for _, name := range []string{"first", "second"} {
		if err := os.MkdirAll(filepath.Join(storage, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	files := &cancelWorkspaceFiles{cancel: cancel}
	_, err := (ProjectEvidence{}).OpenWorkspace(ctx, agentapi.WorkspaceRequest{
		Environment: agentapi.NativePathEnvironment{OperatingSystem: platform.Linux, Locations: agentapi.NativeLocations{UserHome: home}},
		Files:       files, ResolvePath: func(path string) string { return path },
	})
	if !errors.Is(err, context.Canceled) || files.reads != 0 {
		t.Fatalf("workspace cancellation: metadata reads=%d err=%v", files.reads, err)
	}
}
