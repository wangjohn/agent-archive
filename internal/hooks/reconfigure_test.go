package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// An intermediate native removal must not discard the permissions of a file
// that the same atomic setup change keeps for the next selected owner.
func TestReconfigurationNativeOwnerSwitchKeepsExistingMode(t *testing.T) {
	t.Parallel()
	t.Run("genuinely-absent", func(t *testing.T) {
		t.Parallel()
		files := Files{"codex": filepath.Join(t.TempDir(), "settings")}
		hook := Hook{Ports: testPorts, Executable: "/agent-archive", DataHome: t.TempDir()}
		changes, err := PlanReconfiguration(files, nil, hook, hook, []string{"codex"}, nil)
		must(t, err)
		if len(changes) != 1 || changes[0].Existed || changes[0].Mode != 0600 {
			t.Fatal("genuinely absent native destination lost creation defaults")
		}
	})
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-path", true: "symlink"}[alias], func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "settings")
			if err := os.WriteFile(path, nil, 0640); err != nil {
				t.Fatal(err)
			}
			nextPath := path
			if alias {
				nextPath = path + "-alias"
				if err := os.Symlink(path, nextPath); err != nil {
					t.Fatal(err)
				}
			}
			hook := Hook{Ports: testPorts, Executable: "/agent-archive", DataHome: t.TempDir()}
			previous := Files{"claude": path}
			initial, err := Plan(previous, hook, []string{"claude"})
			must(t, err)
			must(t, Apply(initial))
			retirement, found, err := PlanRemovalOf(previous, hook, "claude")
			must(t, err)
			if !found || !retirement.Delete {
				t.Fatal("native removal should delete the owned-only regular file")
			}
			files := Files{"codex": nextPath}
			changes, err := PlanReconfiguration(files, previous, hook, hook, []string{"codex"}, []string{"claude"})
			must(t, err)
			if len(changes) != 1 {
				t.Fatalf("owner switch planned %d changes", len(changes))
			}
			if changes[0].Delete || changes[0].Mode != 0640 {
				t.Fatalf("atomic owner switch: delete=%t mode=%04o", changes[0].Delete, changes[0].Mode)
			}
			must(t, Apply(changes))
			installed, err := Installed(files, hook, "codex")
			must(t, err)
			info, err := os.Stat(path)
			must(t, err)
			if !installed || info.Mode().Perm() != 0640 {
				t.Fatal("next native owner or existing permissions lost")
			}
			if !alias {
				retirement, err := PlanReconfiguration(nil, files, hook, hook, nil, []string{"codex"})
				must(t, err)
				if len(retirement) != 1 || !retirement[0].Delete {
					t.Fatal("pure native deselection no longer deletes an owned-only regular file")
				}
			}
			if alias {
				info, err := os.Lstat(nextPath)
				must(t, err)
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("native settings alias replaced")
				}
			}
		})
	}
}
