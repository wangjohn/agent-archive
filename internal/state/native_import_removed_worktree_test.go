package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// withRemovedWorktreeSource moves a native fixture's recorded working
// directory into a worktree of its project that no longer exists.
func withRemovedWorktreeSource(t *testing.T, f nativeAdmissionFixture) (nativeAdmissionFixture, string) {
	t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(f.candidate.ProjectRoot, ".claude", "worktrees", "removed-task")
	if err := os.MkdirAll(filepath.Dir(removed), 0o700); err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(f.candidate.ProjectRoot), []byte(removed))
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f, removed
}

func TestNativeImportFilesARemovedWorktreeUnderItsProvedProject(t *testing.T) {
	t.Parallel()
	native, removed := withRemovedWorktreeSource(t, newNativeAdmissionFixture(t))
	f := nativeImportPrepareFixtureFromNative(t, native, "", 8<<20)
	cfg, _, err := config.Load(f.native.store.home)
	if err != nil {
		t.Fatal(err)
	}
	proof, proved := sourcefacts.RemovedWorktreeProject(cfg.Archive.Projects, removed)
	if !proved {
		t.Fatal("synthetic removed worktree not proved")
	}
	// Without the proof, the confirmed project is not this source's.
	if p, err := f.prepare(t.Context()); err == nil {
		_ = p.Close() // Already failed the assertion; the close result adds nothing.
		t.Fatal("removed worktree imported without proof")
	}
	f.candidate.RemovedWorktree = &proof
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, err := f.native.store.CommitNativeImport(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ProjectRoot != f.candidate.ProjectRoot || owner.ProjectResolution == nil || *owner.ProjectResolution != proof || owner.NativeBinding.Cwd != removed {
		t.Fatalf("owner project = %q resolution = %+v", owner.ProjectRoot, owner.ProjectResolution)
	}
}

func TestNativeImportRefusesARemovedWorktreeProofThatNoLongerHolds(t *testing.T) {
	t.Parallel()
	native, removed := withRemovedWorktreeSource(t, newNativeAdmissionFixture(t))
	f := nativeImportPrepareFixtureFromNative(t, native, "", 8<<20)
	cfg, _, err := config.Load(f.native.store.home)
	if err != nil {
		t.Fatal(err)
	}
	proof, proved := sourcefacts.RemovedWorktreeProject(cfg.Archive.Projects, removed)
	if !proved {
		t.Fatal("synthetic removed worktree not proved")
	}
	// In order: the last case restores the worktree for good.
	for _, c := range []struct {
		name   string
		change func() archive.ProjectResolution
	}{
		{"another root", func() archive.ProjectResolution { forged := proof; forged.Root = "/elsewhere"; return forged }},
		{"another cwd", func() archive.ProjectResolution {
			forged := proof
			forged.OriginalCwd = filepath.Join(filepath.Dir(removed), "other-task")
			return forged
		}},
		{"the worktree came back", func() archive.ProjectResolution {
			if err := os.Mkdir(removed, 0o700); err != nil {
				t.Fatal(err)
			}
			return proof
		}},
	} {
		name := c.name
		forged := c.change()
		f.candidate.RemovedWorktree = &forged
		if p, err := f.prepare(t.Context()); err == nil {
			_ = p.Close() // Already failed the assertion; the close result adds nothing.
			t.Errorf("%s: stale removed-worktree proof accepted", name)
		}
	}
}
