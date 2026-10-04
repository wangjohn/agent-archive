package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Real hook payloads carry the reported cwd without canonicalizing it. A
// missing worktree descendant still needs the saved checkout's scope decision.
func TestHookScopeResolvesAbsentDescendantsThroughCheckoutAlias(t *testing.T) {
	t.Parallel()
	for _, included := range []bool{true, false} {
		name := "excluded"
		if included {
			name = "included"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(project, alias); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			// The configured child is itself absent. Canonical and alias
			// spellings must agree on its nearest-ancestor decision.
			child := filepath.Join(project, "worktree")
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: child, ProjectID: archive.ProjectID(child), Included: included, ActivatedAt: at.Add(-time.Hour)})
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(alias, "worktree", "not-created")
			if err := HandleEvent(home, "claude", claudeStart(cwd, "native-1", "startup", ""), at, WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if included {
				if len(regs) != 1 || regs[0].ProjectRoot != child || regs[0].ProjectID != archive.ProjectID(child) {
					t.Fatalf("alias cwd lost the configured child identity: %+v", regs)
				}
			} else if len(regs) != 0 {
				t.Fatalf("alias cwd captured through excluded child: %+v", regs)
			}
		})
	}
}

func TestConfiguredScopeResolvesAbsentAliasSubtreesWithoutEscaping(t *testing.T) {
	t.Parallel()
	parent, outside := local.CanonicalPath(t.TempDir()), local.CanonicalPath(t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "escape")); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(alias, "private")
	public := filepath.Join(private, "public")
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{
		{Root: parent, Included: true}, {Root: private, Included: false}, {Root: public, Included: true},
	}}}
	for _, tc := range []struct {
		path, owner string
		included    bool
	}{
		{filepath.Join(parent, "private", "chat"), private, false},
		{filepath.Join(parent, "private", "public", "chat"), public, true},
	} {
		got, found := ConfiguredProjectActivationFor(cfg, tc.path)
		if !found || got.Root != tc.owner || got.Included != tc.included {
			t.Fatalf("absent descendant %s: found=%t project=%+v", tc.path, found, got)
		}
	}
	if got, found := ConfiguredProjectActivationFor(cfg, filepath.Join(alias, "escape", "absent")); found {
		t.Fatalf("symlink escape inherited checkout inclusion: %+v", got)
	}
	if intentProjectStillOwned(parent, cfg.Archive.Projects) {
		t.Fatal("parent admission intent survived an absent nested scope rule")
	}
}

func TestConfiguredScopeRefusesUnresolvedSymlinkIdentity(t *testing.T) {
	t.Parallel()
	parent := local.CanonicalPath(t.TempDir())
	broken := filepath.Join(parent, "broken")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), broken); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: parent, Included: true}}}}
	if got, found := ConfiguredProjectActivationFor(cfg, filepath.Join(broken, "child")); found {
		t.Fatalf("unresolved candidate inherited inclusion: %+v", got)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: broken, Included: false})
	if got, found := ConfiguredProjectActivationFor(cfg, filepath.Join(parent, "absent")); found {
		t.Fatalf("unresolved saved exclusion was ignored: %+v", got)
	}
	if intentProjectStillOwned(parent, cfg.Archive.Projects) {
		t.Fatal("admission intent retained with unresolved scope identity")
	}
}
