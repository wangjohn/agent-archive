package gitremote

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestProjectIdentityTracksBranchConditionalConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/original")
	include := filepath.Join(t.TempDir(), "branch.cfg")
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/branch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[includeIf \"onbranch:selected\"]\n path = " + include + "\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	id := ProjectIdentity(t.Context(), root)
	if !id.Known || !ProjectIdentityCurrent(id) {
		t.Fatalf("initial identity unavailable: %+v", id)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/selected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("branch conditional origin changed without invalidating cached identity")
	}
	if changed := ProjectIdentity(t.Context(), root); !changed.Known || changed.Key != archive.RepoKey("https://example.test/acme/branch") {
		t.Fatalf("changed identity: %+v", changed)
	}
}

func TestProjectIdentityTracksEmptyIncludeConfiguration(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/acme/original")
	include := filepath.Join(t.TempDir(), "empty.cfg")
	if err := os.WriteFile(include, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[include]\n path = " + include + "\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	id := ProjectIdentity(t.Context(), root)
	if !id.Known || !ProjectIdentityCurrent(id) {
		t.Fatalf("initial identity unavailable: %+v", id)
	}
	if err := os.WriteFile(include, []byte("[remote \"origin\"]\n url = https://example.test/acme/changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("empty included file changed without invalidating cached identity")
	}
}

func TestProjectIdentityTracksScratchAncestor(t *testing.T) {
	gitOrSkip(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "scratch")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	id := ProjectIdentity(t.Context(), root)
	if !id.Known || !ProjectIdentityCurrent(id) {
		t.Fatal(id)
	}
	if err := os.Mkdir(filepath.Join(parent, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("scratch gained a Git ancestor without invalidating known absence")
	}
}

// A newly created top-level config can change origin even when it contributed
// no keys to Git's original --show-origin inventory.
func TestProjectIdentityTracksAbsentGlobalConfiguration(t *testing.T) {
	for _, path := range []string{".gitconfig", ".config/git/config"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%v", path, empty), func(t *testing.T) {
				git := gitOrSkip(t)
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
				configPath := filepath.Join(home, path)
				if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
					t.Fatal(err)
				}
				if empty {
					if err := os.WriteFile(configPath, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				root := initRepo(t, git, "")
				id := ProjectIdentity(t.Context(), root)
				if !id.Known || id.Key != "" || !ProjectIdentityCurrent(id) {
					t.Fatalf("initial identity unavailable: %+v", id)
				}
				if err := os.WriteFile(configPath, []byte("[remote \"origin\"]\n url = https://example.test/acme/new\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if ProjectIdentityCurrent(id) {
					t.Fatal("new global configuration did not invalidate known absence")
				}
				if changed := ProjectIdentity(t.Context(), root); !changed.Known || changed.Key != archive.RepoKey("https://example.test/acme/new") {
					t.Fatalf("changed identity: %+v", changed)
				}
			})
		}
	}
}
