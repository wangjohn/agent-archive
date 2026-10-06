package gitremote

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"os/exec"
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

func TestTopLevelConfigDependenciesRespectGitPathQueryContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		global    string
		system    string
		globalErr error
		systemErr error
		want      bool
		count     int
	}{
		{name: "multiple candidate paths", global: "/synthetic/xdg/git/config\n/synthetic/home/.gitconfig\n", system: "/synthetic/etc/gitconfig\n", want: true, count: 3},
		{name: "disabled system", global: "/synthetic/home/.gitconfig\n", systemErr: projectExitStatusError(1), want: true, count: 1},
		{name: "no global paths", globalErr: projectExitStatusError(1), system: "/synthetic/etc/gitconfig\n", want: true, count: 1},
		{name: "both no value", globalErr: projectExitStatusError(1), systemErr: projectExitStatusError(1), want: true},
		{name: "unsupported variable", globalErr: projectExitStatusError(129)},
		{name: "failed output", global: "/synthetic/home/.gitconfig\n", globalErr: projectExitStatusError(1)},
		{name: "relative path", global: "relative\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if args[len(args)-1] == "GIT_CONFIG_GLOBAL" {
					return []byte(tc.global), tc.globalErr
				}
				return []byte(tc.system), tc.systemErr
			}
			paths := map[string]bool{}
			if got := topLevelConfigDependencies(t.Context(), "/synthetic/repo", paths, run); got != tc.want || (got && len(paths) != tc.count) {
				t.Fatal(got, paths)
			}
		})
	}
}

func TestProjectIdentityTracksNewNestedRepositoryAncestor(t *testing.T) {
	git := gitOrSkip(t)
	target := initRepo(t, git, "https://example.test/acme/target")
	outer := initRepo(t, git, "https://example.test/acme/outer")
	nested := filepath.Join(outer, "nested")
	configured := filepath.Join(nested, "configured")
	if err := os.MkdirAll(configured, 0700); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://example.test/acme/target")
	projects := []archive.ProjectActivation{{Root: target, Included: true}, {Root: configured, Included: false}}
	resolve := func(p string) string {
		v, e := filepath.EvalSymlinks(p)
		if e != nil {
			return filepath.Clean(p)
		}
		return v
	}
	r := sourcefacts.NewRecoveryResolver(projects, nil, resolve, ProjectIdentity, nil)
	r.Validate = ProjectIdentityCurrent
	gone := filepath.Join(t.TempDir(), "gone")
	proof, outcome := r.Recover(t.Context(), gone, key)
	if outcome != "" || !r.Current(proof) {
		t.Fatalf("initial %+v %s", proof, outcome)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://example.test/acme/target"}} {
		cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", nested}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, output)
		}
	}
	fresh := ProjectIdentity(t.Context(), configured)
	if !fresh.Known || fresh.Key != key {
		t.Fatalf("fresh nested clone invalid: %+v", fresh)
	}
	freshResolver := sourcefacts.NewRecoveryResolver(projects, nil, resolve, ProjectIdentity, nil)
	freshResolver.Validate = ProjectIdentityCurrent
	_, freshOutcome := freshResolver.Recover(t.Context(), gone, key)
	if freshOutcome != sourcefacts.RecoverySubtreeUnavailable {
		t.Fatalf("fresh outcome %s", freshOutcome)
	}
	if r.Current(proof) {
		t.Fatalf("stale proof still certifies target %s despite newly excluded nested clone; fresh inventory yields %s", proof.Root, freshOutcome)
	}
	if _, outcome := r.Recover(t.Context(), gone, key); outcome != sourcefacts.RecoveryInventoryUnavailable {
		t.Fatalf("stale cached uniqueness must stay pending: %s", outcome)
	}
}
