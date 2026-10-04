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

// Native cwd casing is not an identity boundary on a case-insensitive volume.
// In particular, missing descendants must still inherit a saved exclusion.
func TestHookScopePreservesRulesAcrossCaseInsensitiveSpellings(t *testing.T) {
	t.Parallel()
	project := local.CanonicalPath(t.TempDir())
	private := filepath.Join(project, "Private")
	public := filepath.Join(private, "Public")
	if err := os.MkdirAll(public, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, "private", "public")); err != nil {
		t.Skip("requires a case-insensitive volume")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, owner string
		included         bool
	}{
		{"excluded existing", filepath.Join(project, "private"), private, false},
		{"excluded absent", filepath.Join(alias, "private", "absent"), private, false},
		{"reincluded existing", filepath.Join(project, "private", "public"), public, true},
		{"reincluded absent", filepath.Join(alias, "private", "public", "absent"), public, true},
		{"absent excluded rule", filepath.Join(alias, "private", "public", "absentexcluded", "chat"), filepath.Join(public, "AbsentExcluded"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Archive.Projects = append(cfg.Archive.Projects,
				archive.ProjectActivation{Root: private, ProjectID: archive.ProjectID(private), Included: false},
				archive.ProjectActivation{Root: public, ProjectID: archive.ProjectID(public), Included: true, ActivatedAt: at.Add(-time.Hour)},
				archive.ProjectActivation{Root: filepath.Join(public, "AbsentExcluded"), Included: false},
			)
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			if err := HandleEvent(home, "claude", claudeStart(tc.cwd, "native-1", "startup", ""), at, WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.included {
				if len(regs) != 0 {
					t.Fatalf("case alias bypassed saved exclusion: %+v", regs)
				}
			} else if len(regs) != 1 || regs[0].ProjectRoot != tc.owner || regs[0].ProjectID != archive.ProjectID(tc.owner) {
				t.Fatalf("case alias lost saved reinclusion identity: %+v", regs)
			}
		})
	}
	t.Run("prunes ambiguous parent retry", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
		setUpTestConfig(t, home, private, at.Add(-time.Hour))
		cfg, _, err := config.Load(home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(alias, "private", "public", "absent"), Included: false})
		if err := os.MkdirAll(admissionIntentDir(home), 0700); err != nil {
			t.Fatal(err)
		}
		intentPath := filepath.Join(admissionIntentDir(home), "retry.json")
		if err := local.Write(intentPath, admissionIntent{ProjectRoot: private, DestinationID: cfg.DestinationID(), ObservedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := PruneAdmissionIntents(home, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
			t.Fatalf("ambiguous parent intent survived a differently cased nested exclusion: %v", err)
		}
	})
}

// Existing directories with different case remain distinct on a sensitive
// volume; conservative absent-path ambiguity must not merge these identities.
func TestHookScopeKeepsExistingCaseSensitivePathsDistinct(t *testing.T) {
	t.Parallel()
	project := local.CanonicalPath(t.TempDir())
	private, other := filepath.Join(project, "Private"), filepath.Join(project, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0700); os.IsExist(err) {
		t.Skip("requires a case-sensitive volume")
	} else if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: private, Included: false})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "claude", claudeStart(filepath.Join(other, "absent"), "native-1", "startup", ""), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || regs[0].ProjectRoot != project {
		t.Fatalf("distinct existing path inherited another directory's exclusion: %+v", regs)
	}
}

// No filesystem can prove two absent case variants distinct by file identity.
// This portable check pins the conservative decline, including queue pruning.
func TestHookScopeDeclinesAmbiguousAbsentCaseVariants(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), local.CanonicalPath(t.TempDir())
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(project, "AbsentPrivate"), Included: false})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "claude", claudeStart(filepath.Join(project, "absentprivate", "chat"), "native-1", "startup", ""), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("ambiguous absent variant bypassed exclusion: %+v", regs)
	}
	// Pruning cannot retain an included absent owner when a differently cased
	// absent rule might revoke its ownership.
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: filepath.Join(project, "AbsentOwner"), Included: true, ActivatedAt: at.Add(-time.Hour)}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(cfg.Archive.Projects[0].Root, "retry-native", "startup", ""), at)
	if err != nil || !queued {
		t.Fatalf("queue original proven start: queued=%t err=%v", queued, err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(project, "absentowner", "private"), Included: false})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second), testDecoders); err != nil {
		t.Fatal(err)
	}
	regs, err = state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("ambiguous absent owner replayed a revoked start: %+v", regs)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("revoked intent was retained for later publication: %v", entries)
	}
	if err := os.MkdirAll(admissionIntentDir(home), 0700); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(admissionIntentDir(home), "retry.json")
	if err := local.Write(intentPath, admissionIntent{ProjectRoot: cfg.Archive.Projects[0].Root, DestinationID: cfg.DestinationID(), ObservedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := PruneAdmissionIntents(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Fatalf("ambiguous absent owner retained retry intent: %v", err)
	}
}
