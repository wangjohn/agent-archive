package cli

import (
	"bytes"
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
			output, err := exec.CommandContext(t.Context(), "sh", "-c", "set -- "+command+"; printf '%s\\000' \"$@\"").Output()
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
		var projects []archive.ProjectActivation
		other := filepath.Join(home, "other")
		if reason == "ambiguous" {
			must(t, os.Mkdir(other, 0700))
			projects = []archive.ProjectActivation{{Root: other, Included: true}}
		} else {
			env.repoKeyContext = func(context.Context, string) string { return "" }
		}
		if reason == "ambiguous" {
			env.repoKeyContext = func(context.Context, string) string { return key }
		}
		cfg := config.Config{Archive: archive.Config{Projects: projects}}
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

func TestScopeTransferRefusesSavedReinclusionUnderTransferredExclusion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	private := filepath.Join(root, "private")
	saved := filepath.Join(private, "saved")
	must(t, os.MkdirAll(saved, 0700))
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: saved, Included: true}}}}
	before := append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
	encoded, err := json.Marshal([]portableProjectRule{{Path: root, Included: true}, {Path: private, Included: false}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) == 0 {
		activation, _ := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(saved, "chat"))
		t.Fatalf("accepted saved reinclusion defeating transferred exclusion: %+v", activation)
	}
	if !reflect.DeepEqual(before, cfg.Archive.Projects) {
		t.Fatal("refusal changed saved capture decisions")
	}
	// An explicit source reinclusion authorizes the saved subtree.
	encoded, err = json.Marshal([]portableProjectRule{{Path: root, Included: true}, {Path: private, Included: false}, {Path: saved, Included: true}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) != 0 {
		t.Fatal(problems)
	}
	activation, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(saved, "chat"))
	if !found || !activation.Included {
		t.Fatal("explicit reinclusion lost")
	}
}

func TestScopeTransferUpdatesSavedCanonicalAlias(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	private := filepath.Join(root, "private")
	must(t, os.MkdirAll(private, 0700))
	alias := filepath.Join(home, "alias")
	must(t, os.Symlink(private, alias))
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: alias, Included: true}}}}
	encoded, err := json.Marshal([]portableProjectRule{{Path: root, Included: true}, {Path: private, Included: false}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, candidate := range []string{private, alias} {
		activation, found := capture.ConfiguredProjectActivationFor(cfg, candidate)
		if !found || activation.Included {
			t.Fatalf("saved alias defeated exclusion at %s: %+v", candidate, activation)
		}
	}
	if len(cfg.Archive.Projects) != 2 {
		t.Fatalf("duplicate saved identity: %+v", cfg.Archive.Projects)
	}
}

func TestPortableScopeKeepsCanonicalAliasSubtreeAttached(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	private := filepath.Join(root, "private")
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0700))
	must(t, os.MkdirAll(private, 0700))
	alias := filepath.Join(home, "alias")
	must(t, os.Symlink(root, alias))
	key := archive.RepoKey("https://example.test/team/repo.git")
	env := Env{repoKeyContext: func(context.Context, string) string { return key }}
	encoded := portableProjectScope([]archive.ProjectActivation{{Root: alias, Included: true}, {Root: private, Included: false}}, home, env, context.Background())
	var rules []portableProjectRule
	must(t, json.Unmarshal([]byte(encoded), &rules))
	if len(rules) != 2 || rules[0].RepoKey != key || rules[1].RepoKey != key || rules[1].Path != "private" {
		t.Fatalf("alias detached exclusion: %s", encoded)
	}
}

func TestScopeTransferPreservesSavedExclusionsAndOriginalConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	private := filepath.Join(root, "private")
	must(t, os.MkdirAll(private, 0700))
	existing := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}, {Root: private, Included: false}}}}
	cfg := existing
	before := append([]archive.ProjectActivation(nil), existing.Archive.Projects...)
	encoded, err := json.Marshal([]portableProjectRule{{Path: root, Included: true}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) != 0 {
		t.Fatal(problems)
	}
	activation, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(private, "chat"))
	if !found || activation.Included {
		t.Fatal("saved descendant exclusion lost")
	}
	encoded, err = json.Marshal([]portableProjectRule{{Path: private, Included: true}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) == 0 {
		t.Fatal("silently overrode destination exclusion")
	}
	if !reflect.DeepEqual(before, cfg.Archive.Projects) {
		t.Fatal("refusal changed scope")
	}
	// A successful explicit exclusion changes the candidate, not the saved
	// config that setup subsequently uses to review consent changes.
	encoded, err = json.Marshal([]portableProjectRule{{Path: root, Included: false}})
	must(t, err)
	if problems := setupProjectScope(&cfg, string(encoded), home, Env{}); len(problems) != 0 {
		t.Fatal(problems)
	}
	if !reflect.DeepEqual(before, existing.Archive.Projects) {
		t.Fatal("candidate mutated original saved scope")
	}
	activation, found = capture.ConfiguredProjectActivationFor(cfg, root)
	if !found || activation.Included {
		t.Fatal("explicit exclusion was not applied")
	}
}

func TestScopeTransferRejectsInvalidOrDuplicateRulesAtomically(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	must(t, os.Mkdir(root, 0700))
	alias := filepath.Join(home, "alias")
	must(t, os.Symlink(root, alias))
	good, err := json.Marshal([]portableProjectRule{{Path: root, Included: true}})
	must(t, err)
	duplicate, err := json.Marshal([]portableProjectRule{{Path: root, Included: true}, {Path: alias, Included: false}})
	must(t, err)
	for _, encoded := range []string{"null", "[]", "{}", `[{"path":"~"}]`, `[{"path":"~","included":null}]`, `[{"path":"relative","included":false}]`, `[{"path":"~","included":true,"unexpected":1}]`, string(good) + " []", string(duplicate)} {
		cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: false}}}}
		before := append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		if problems := setupProjectScope(&cfg, encoded, home, Env{}); len(problems) == 0 {
			t.Fatalf("accepted invalid scope %s", encoded)
		}
		if !reflect.DeepEqual(before, cfg.Archive.Projects) {
			t.Fatal("invalid input partially changed capture scope")
		}
	}
}

func TestScopeTransferRefusesIncompleteAndSavedBlockedRepositories(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	must(t, os.Mkdir(root, 0700))
	key := archive.RepoKey("https://example.test/team/repo.git")
	encoded, err := json.Marshal([]portableProjectRule{{RepoKey: key, Path: ".", Included: true}, {RepoKey: key, Path: "private", Included: false}})
	must(t, err)
	for _, incomplete := range []bool{false, true} {
		cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: false}}}}
		before := append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		env := Env{LookupEnv: func(string) (string, bool) { return "", false }, WorkingDir: func() (string, error) { return root, nil }, repoKeyContext: func(context.Context, string) string {
			if incomplete {
				return "unknown-origin"
			}
			return key
		}}
		problems := setupProjectScope(&cfg, string(encoded), home, env)
		if len(problems) == 0 {
			t.Fatal("accepted incomplete or saved-excluded repository")
		}
		if incomplete && !strings.Contains(problems[0].Error(), "incomplete") {
			t.Fatalf("not classified as incomplete: %v", problems)
		}
		if !reflect.DeepEqual(before, cfg.Archive.Projects) {
			t.Fatal("failed matching changed saved capture decisions")
		}
	}
}

func TestScopeTransferRejectsUnresolvedSymlinkBeforeInclusion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	must(t, os.Mkdir(root, 0700))
	key := archive.RepoKey("https://example.test/team/repo.git")
	env := Env{LookupEnv: noEnv, repoKeyContext: func(context.Context, string) string { return key }, WorkingDir: func() (string, error) { return root, nil }}
	for _, target := range []string{filepath.Join(t.TempDir(), "not-created"), "unresolved"} {
		link := filepath.Join(root, "unresolved")
		must(t, os.Symlink(target, link))
		for _, keyed := range []bool{false, true} {
			cfg := config.Config{}
			rules := []portableProjectRule{{Path: root, Included: true}, {Path: filepath.Join(link, "private"), Included: false}}
			if keyed {
				rules = []portableProjectRule{{RepoKey: key, Path: ".", Included: true}, {RepoKey: key, Path: "unresolved/private", Included: false}}
			}
			encoded, err := json.Marshal(rules)
			must(t, err)
			if problems := setupProjectScope(&cfg, string(encoded), home, env); len(problems) == 0 {
				t.Fatalf("accepted unresolved link %q (keyed=%t)", target, keyed)
			}
			if len(cfg.Archive.Projects) != 0 {
				t.Fatalf("applied inclusion without resolved exclusion: %+v", cfg.Archive.Projects)
			}
		}
		must(t, os.Remove(link))
	}
}

func TestPrintedScopeRequiresActualRepositoryTopLevel(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	top := filepath.Join(home, "repo")
	child := filepath.Join(top, "pkg")
	must(t, os.MkdirAll(filepath.Join(child, ".git"), 0700))
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is unavailable")
	}
	gitHome := t.TempDir()
	runGit := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + gitHome, "GIT_CONFIG_NOSYSTEM=1"}
		return cmd.Output()
	}
	for _, args := range [][]string{{"init", "-q", top}, {"-C", top, "config", "remote.origin.url", "https://example.test/team/repo.git"}} {
		output, err := runGit(t.Context(), "", args...)
		if err != nil {
			t.Fatalf("synthetic repository: %v: %s", err, output)
		}
	}
	env := Env{projectGitRunner: runGit}
	projects := []archive.ProjectActivation{{Root: child, Included: true}, {Root: filepath.Join(child, "private"), Included: false}}
	encoded := portableProjectScope(projects, home, env, t.Context())
	if strings.Contains(encoded, "repo_key") {
		t.Fatalf("inherited origin widened subtree to full repository: %s", encoded)
	}
	var rules []portableProjectRule
	must(t, json.Unmarshal([]byte(encoded), &rules))
	if rules[0].Path != "~/repo/pkg" || rules[1].Path != "~/repo/pkg/private" {
		t.Fatalf("changed subtree scope: %+v", rules)
	}
}

func TestPairingRefusesProjectScopeBeforeReadingBundle(t *testing.T) {
	t.Parallel()
	env := Env{LookupEnv: noEnv, Home: func() (string, error) { t.Fatal("read home"); return "", nil }, PairingCode: func() (string, error) { t.Fatal("read pairing code"); return "", nil }}
	var out bytes.Buffer
	if code := Run([]string{"setup", "--pair-file", "-", "--yes", "--project-scope", `[{"path":"~/private","included":false}]`}, strings.NewReader("malformed bundle"), &out, &out, env); code != 2 || !strings.Contains(out.String(), "pairing accepts") {
		t.Fatalf("scope was silently ignored by pairing: exit %d, %s", code, &out)
	}
}

func TestPrintedScopeKeepsDistinctClonesWithDifferentExclusions(t *testing.T) {
	t.Parallel()
	sourceHome, destinationHome := t.TempDir(), t.TempDir()
	var projects []archive.ProjectActivation
	for _, name := range []string{"first", "second"} {
		for _, home := range []string{sourceHome, destinationHome} {
			must(t, os.MkdirAll(filepath.Join(home, name, ".git"), 0700))
		}
		root := filepath.Join(sourceHome, name)
		projects = append(projects, archive.ProjectActivation{Root: root, Included: true}, archive.ProjectActivation{Root: filepath.Join(root, name+"-private"), Included: false})
	}
	key := archive.RepoKey("https://example.test/team/repo.git")
	env := Env{LookupEnv: noEnv, repoKeyContext: func(context.Context, string) string { return key }}
	encoded := portableProjectScope(projects, sourceHome, env, t.Context())
	if strings.Contains(encoded, "repo_key") {
		t.Fatalf("collapsed distinct clone scopes onto one identity: %s", encoded)
	}
	cfg := config.Config{}
	if problems := setupProjectScope(&cfg, encoded, destinationHome, env); len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, name := range []string{"first", "second"} {
		for _, sample := range []struct {
			path     string
			included bool
		}{{"ordinary", true}, {name + "-private/chat", false}} {
			activation, found := capture.ConfiguredProjectActivationFor(cfg, filepath.Join(destinationHome, name, sample.path))
			if !found || activation.Included != sample.included {
				t.Fatalf("lost %s clone scope for %s: %+v", name, sample.path, activation)
			}
		}
	}
}
