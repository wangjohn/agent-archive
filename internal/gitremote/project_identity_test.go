package gitremote

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectIdentityTracksIncludedConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "")
	include := filepath.Join(t.TempDir(), "remote.cfg")
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://user:synthetic-secret@example.test/acme/repo.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".git", "config")
	if err := os.WriteFile(config, []byte("[core]\n repositoryformatversion = 0\n bare = false\n[include]\n path = "+include+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := ProjectIdentity(context.Background(), root)
	if !id.Known || id.Key != archive.RepoKey("git@example.test:acme/repo.git") || !ProjectIdentityCurrent(id) {
		t.Fatalf("%+v", id)
	}
	raw, err := json.Marshal(id)
	if err != nil || strings.Contains(string(raw), "synthetic-secret") || strings.Contains(string(raw), "https://") {
		t.Fatal("raw URL entered cached evidence")
	}
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/other.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("included remote change left stale identity")
	}
	changed := ProjectIdentity(context.Background(), root)
	if !changed.Known || changed.Key == id.Key {
		t.Fatal(changed)
	}
}

func TestProjectIdentitySeparatesScratchAndUnreadableClone(t *testing.T) {
	gitOrSkip(t)
	scratch := t.TempDir()
	if id := ProjectIdentity(context.Background(), scratch); !id.Known || id.Root != "" {
		t.Fatal(id)
	}
	if id := ProjectIdentity(context.Background(), filepath.Join(t.TempDir(), "gone")); id.Known {
		t.Fatal("missing clone classified as scratch")
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("gitdir: /synthetic/deleted-gitdir\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if id := ProjectIdentity(context.Background(), broken); id.Known {
		t.Fatal("broken clone classified as scratch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id := ProjectIdentity(ctx, scratch); id.Known {
		t.Fatal("cancelled Git observation became evidence")
	}
}
