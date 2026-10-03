package sourceio

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestStrictFileProviderSignatureAndReadKeepRootConfinement(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "native.jsonl")
	external := filepath.Join(outside, "native.jsonl")
	for _, path := range []string{inside, external} {
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	leaf := filepath.Join(root, "leaf.jsonl")
	if err := os.Symlink(inside, leaf); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "escape")
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	internal := filepath.Join(root, "internal")
	if err := os.Symlink(".", internal); err != nil {
		t.Fatal(err)
	}
	absoluteInternal := filepath.Join(root, "absolute-internal")
	if err := os.Symlink(root, absoluteInternal); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	pass, err := (FileProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: sourcefacts.RootOpener{Root: root}, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	for _, path := range []string{inside, filepath.Join(internal, "native.jsonl"), leaf, external, filepath.Join(parent, "native.jsonl"), filepath.Join(absoluteInternal, "native.jsonl"), fifo} {
		safe := path == inside || path == filepath.Join(internal, "native.jsonl")
		if _, err := pass.Signature(t.Context(), agentapi.SourceRef{Path: path}); (err == nil) != safe {
			t.Fatalf("strict signature safe=%v err=%v", safe, err)
		}
		snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
		if (err == nil) != safe {
			t.Fatalf("strict read safe=%v err=%v", safe, err)
		}
		if snapshot != nil {
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
