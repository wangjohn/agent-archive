package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestPrintedScopePreservesExclusionsAcrossFreshMachines(t *testing.T) {
	t.Parallel()
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "home paths", true: "relocated checkout"}[keyed], func(t *testing.T) {
			t.Parallel()
			sourceHome, destinationHome := t.TempDir(), t.TempDir()
			sourceRoot := filepath.Join(sourceHome, "src", "repo")
			destinationRoot := filepath.Join(destinationHome, "src", "repo")
			if keyed {
				destinationRoot = filepath.Join(destinationHome, "different-checkout")
			}
			for _, root := range []string{sourceRoot, destinationRoot} {
				must(t, os.MkdirAll(filepath.Join(root, "private", "public"), 0700))
				if keyed {
					must(t, os.Mkdir(filepath.Join(root, ".git"), 0700))
					must(t, os.Mkdir(filepath.Join(root, "private", ".git"), 0700))
				}
			}
			projects := []archive.ProjectActivation{
				{Root: sourceRoot, Included: true},
				{Root: filepath.Join(sourceRoot, "private"), Included: false},
				{Root: filepath.Join(sourceRoot, "private", "public"), Included: true},
				{Root: filepath.Join(sourceRoot, "future-private"), Included: false},
			}
			key := archive.RepoKey("https://example.test/team/repo.git")
			env := Env{LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(_ context.Context, path string) string {
				if keyed && filepath.Base(path) == "private" {
					return archive.RepoKey("https://example.test/team/private.git")
				}
				if keyed {
					return key
				}
				return ""
			}, WorkingDir: func() (string, error) { return destinationRoot, nil }}
			command := anotherMachineCommand(config.Config{Archive: archive.Config{Projects: projects}}, sourceHome, env)
			// Execute only shell argument parsing, so this also verifies the
			// printed JSON survives quotes and reaches the receiving flag intact.
			output, err := exec.Command("sh", "-c", "set -- "+command+"; printf '%s\\000' \"$@\"").Output()
			must(t, err)
			args := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
			encoded := ""
			for i, arg := range args {
				if arg == "--project-scope" {
					encoded = args[i+1]
				}
			}
			if encoded == "" {
				t.Fatalf("no transferred scope: %s", command)
			}
			opts, ok := setupFlags(env.newCommandFlags("setup", io.Discard), args[2:])
			if !ok || opts.projectScope != encoded || !opts.given() {
				t.Fatalf("receiving flags dropped scope: %+v", opts)
			}
			cfg := config.Config{}
			if problems := setupProjectScope(&cfg, opts.projectScope, destinationHome, env); len(problems) > 0 {
				t.Fatal(problems)
			}
			if len(cfg.Archive.Projects) != len(projects) {
				t.Fatalf("got %+v", cfg.Archive.Projects)
			}
			for i, rule := range projects {
				rel, err := filepath.Rel(sourceRoot, rule.Root)
				must(t, err)
				got := cfg.Archive.Projects[i]
				if got.Root != filepath.Join(destinationRoot, rel) || got.Included != rule.Included {
					t.Fatalf("rule %d: %+v", i, got)
				}
			}
			for _, sample := range []struct {
				path     string
				included bool
			}{{"ordinary", true}, {"private/chat", false}, {"private/public/chat", true}, {"future-private/chat", false}} {
				activation, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(destinationRoot, sample.path))
				if !found || activation.Included != sample.included {
					t.Fatalf("capture scope for %s: %+v", sample.path, activation)
				}
			}
		})
	}
}

func TestScopeTransferRefusesPartialInclusionsAndEscapingSubtrees(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	must(t, os.Mkdir(root, 0700))
	outside := t.TempDir()
	must(t, os.Symlink(outside, filepath.Join(root, "escape")))
	key := archive.RepoKey("https://example.test/team/repo.git")
	env := Env{LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { return key }, WorkingDir: func() (string, error) { return root, nil }}
	for _, subtree := range []string{"../outside", "/absolute", "escape", ""} {
		cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: outside, Included: false}}}}
		before := append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		encoded, err := json.Marshal([]portableProjectRule{{RepoKey: key, Path: ".", Included: true}, {RepoKey: key, Path: subtree, Included: false}})
		must(t, err)
		if problems := setupProjectScope(&cfg, string(encoded), home, env); len(problems) == 0 {
			t.Fatalf("accepted %q", subtree)
		}
		if !reflect.DeepEqual(before, cfg.Archive.Projects) {
			t.Fatalf("partial inclusion for %q", subtree)
		}
	}
	for _, reason := range []string{"missing", "ambiguous"} {
		cfg := config.Config{}
		other := filepath.Join(home, "other")
		if reason == "ambiguous" {
			must(t, os.Mkdir(other, 0700))
			cfg.Archive.Projects = []archive.ProjectActivation{{Root: other, Included: true}}
		} else {
			env.repoKeyContext = func(context.Context, string) string { return "" }
		}
		if reason == "ambiguous" {
			env.repoKeyContext = func(context.Context, string) string { return key }
		}
		before := append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		encoded, err := json.Marshal([]portableProjectRule{{RepoKey: key, Path: ".", Included: true}, {RepoKey: key, Path: "private", Included: false}})
		must(t, err)
		if problems := setupProjectScope(&cfg, string(encoded), home, env); len(problems) == 0 {
			t.Fatalf("accepted %s clone", reason)
		}
		if !reflect.DeepEqual(before, cfg.Archive.Projects) {
			t.Fatalf("partial inclusion for %s clone", reason)
		}
	}
}

func TestScopeKeepsNestedCheckoutAttachedToPathBasedAncestor(t *testing.T) {
	t.Parallel()
	for _, ancestorIncluded := range []bool{true, false} {
		sourceHome, destinationHome := t.TempDir(), t.TempDir()
		sourceParent := filepath.Join(sourceHome, "src")
		sourceRepo := filepath.Join(sourceParent, "private")
		destinationParent := filepath.Join(destinationHome, "src")
		destinationRepo := filepath.Join(destinationParent, "private")
		relocatedRepo := filepath.Join(destinationHome, "relocated")
		for _, root := range []string{sourceRepo, destinationRepo, relocatedRepo} {
			must(t, os.MkdirAll(filepath.Join(root, ".git"), 0700))
		}
		key := archive.RepoKey("https://example.test/team/private.git")
		env := Env{repoKeyContext: func(context.Context, string) string { return key }, WorkingDir: func() (string, error) { return relocatedRepo, nil }}
		rules := []archive.ProjectActivation{{Root: sourceParent, Included: ancestorIncluded}, {Root: sourceRepo, Included: !ancestorIncluded}}
		encoded := portableProjectScope(rules, sourceHome, env, context.Background())
		if strings.Contains(encoded, "repo_key") {
			t.Fatalf("detached checkout from path-based ancestor: %s", encoded)
		}
		cfg := config.Config{}
		if problems := setupProjectScope(&cfg, encoded, destinationHome, env); len(problems) != 0 {
			t.Fatal(problems)
		}
		activation, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(destinationRepo, "chat"))
		if !found || activation.Included != !ancestorIncluded {
			t.Fatalf("nested decision lost: %+v", activation)
		}
		if _, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(relocatedRepo, "chat")); found {
			t.Fatal("scope rule moved independently of its ancestor")
		}
	}
}
