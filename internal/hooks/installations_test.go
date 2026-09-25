package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

// Two installations in one HOME (the default one and one with its own
// AGENT_ARCHIVE_HOME) share the apps' hook files. Neither's setup (Merge)
// nor uninstall (Remove) may touch the other's handlers, and Installed sees
// each as installed however the other changes.
func TestTwoInstallationsNeverTouchEachOther(t *testing.T) {
	const exe = "/usr/local/bin/agent-archive"
	for _, app := range []string{"claude", "codex", "cursor"} {
		t.Run(app, func(t *testing.T) {
			files := testFiles(t.TempDir())
			primary := Hook{Executable: exe, DefaultDataHome: "/Users/u/.local/share/agent-archive"}
			secondary := Hook{Executable: exe, DataHome: "/tmp/aa-test", DefaultDataHome: primary.DefaultDataHome}
			install := func(h Hook) {
				t.Helper()
				changes, err := Plan(files, h, []string{app})
				must(t, err)
				must(t, Apply(changes))
			}
			remove := func(h Hook) {
				t.Helper()
				changes, err := PlanRemoval(files, h, []string{app})
				must(t, err)
				must(t, Apply(changes))
			}
			installed := func(h Hook) bool {
				t.Helper()
				ok, err := Installed(files, h, app)
				must(t, err)
				return ok
			}
			install(primary)
			install(secondary)
			if !installed(primary) || !installed(secondary) {
				t.Fatalf("after both setups: primary=%v secondary=%v", installed(primary), installed(secondary))
			}
			// Rerunning either setup changes nothing of the other's.
			install(primary)
			if !installed(secondary) {
				t.Fatal("rerunning the default setup took over the second installation's hooks")
			}
			others, err := OtherInstallations(files, primary, app)
			must(t, err)
			if len(others) != 1 || others[0].DataHome != "/tmp/aa-test" {
				t.Fatalf("primary sees %+v", others)
			}
			others, err = OtherInstallations(files, secondary, app)
			must(t, err)
			if len(others) != 1 || others[0].DataHome != primary.DefaultDataHome {
				t.Fatalf("secondary sees %+v", others)
			}
			remove(secondary)
			if !installed(primary) || installed(secondary) {
				t.Fatalf("after the second uninstall: primary=%v secondary=%v", installed(primary), installed(secondary))
			}
			install(secondary)
			remove(primary)
			if installed(primary) || !installed(secondary) {
				t.Fatalf("after the default uninstall: primary=%v secondary=%v", installed(primary), installed(secondary))
			}
			remove(secondary)
			// Setup created the file; with both installations gone, so is it.
			if data, err := os.ReadFile(files[app]); !os.IsNotExist(err) {
				t.Fatalf("a handler was stranded (%v):\n%s", err, data)
			}
		})
	}
}

// A handler is the default installation's whether its command leaves
// AGENT_ARCHIVE_HOME out (as setup writes it) or names the default directory,
// and an installation's directory matches through symlinks.
func TestInstallationIdentityIsTheDataDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "realDir")
	must(t, os.MkdirAll(realDir, 0o700))
	link := filepath.Join(root, "link")
	must(t, os.Symlink(realDir, link))
	defaultHome := filepath.Join(root, "default")
	cases := []struct {
		name     string
		hook     Hook
		dataHome string
		want     bool
	}{
		{"default, implicit", Hook{DefaultDataHome: defaultHome}, "", true},
		{"default, named", Hook{DataHome: defaultHome, DefaultDataHome: defaultHome}, "", true},
		{"default, handler names it", Hook{DefaultDataHome: defaultHome}, defaultHome, true},
		{"default unknown", Hook{}, "", true},
		{"other directory", Hook{DefaultDataHome: defaultHome}, realDir, false},
		{"default versus other", Hook{DataHome: realDir, DefaultDataHome: defaultHome}, "", false},
		{"through a symlink", Hook{DataHome: link}, realDir, true},
		{"sibling prefix", Hook{DataHome: realDir}, realDir + "2", false},
		{"trailing slash", Hook{DataHome: realDir}, realDir + "/", true},
		{"missing directory", Hook{DataHome: filepath.Join(root, "gone")}, filepath.Join(root, "gone"), true},
	}
	// On a case-insensitive volume (macOS's default), a spelling that differs
	// only in case is the same directory, so the same installation.
	if _, err := os.Stat(filepath.Join(root, "REALDIR")); err == nil {
		cases = append(cases, struct {
			name     string
			hook     Hook
			dataHome string
			want     bool
		}{"another case", Hook{DataHome: realDir}, filepath.Join(root, "REALDIR"), true})
	}
	for _, c := range cases {
		if got := c.hook.sameInstallation(c.dataHome); got != c.want {
			t.Errorf("%s: sameInstallation(%q) = %v", c.name, c.dataHome, got)
		}
	}
}

// CommandDataHome reads back exactly the directory Command wrote, whatever
// characters it holds, and refuses anything Command never writes.
func TestCommandDataHomeRoundTrips(t *testing.T) {
	for _, dir := range []string{"", "/tmp/aa-test", "/Users/o'brien/data", "/Volumes/My Disk/a b", `/x/$HOME/"q"/;rm -rf`, "/it's/''/end'"} {
		command, err := Hook{Executable: "/bin/agent-archive", DataHome: dir}.Command("claude")
		must(t, err)
		got, ok := CommandDataHome(command)
		if !ok || got != dir {
			t.Errorf("%q: got %q ok=%v from %s", dir, got, ok, command)
		}
	}
	for _, command := range []string{
		"AGENT_ARCHIVE_HOME=/unquoted '/bin/agent-archive' _hook",
		"AGENT_ARCHIVE_HOME='relative' '/bin/agent-archive' _hook",
		"AGENT_ARCHIVE_HOME='/unterminated",
		"AGENT_ARCHIVE_HOME='/a'",
		"AGENT_ARCHIVE_HOME='/a'x '/bin/agent-archive'",
	} {
		if got, ok := CommandDataHome(command); ok {
			t.Errorf("%q read as %q", command, got)
		}
	}
}

func FuzzCommandDataHome(f *testing.F) {
	f.Add("/tmp/aa-test")
	f.Add("/it's/''/end'")
	f.Fuzz(func(t *testing.T, dir string) {
		command, err := Hook{Executable: "/bin/agent-archive", DataHome: dir}.Command("cursor")
		if err != nil {
			return
		}
		if got, ok := CommandDataHome(command); !ok || got != dir {
			t.Fatalf("%q: got %q ok=%v", dir, got, ok)
		}
	})
}

// A marked handler whose data directory cannot be read (edited by hand)
// belongs to no installation that can prove it: it is reported, and neither
// setup nor uninstall removes it.
func TestUnreadableHandlerIsReportedNotRemoved(t *testing.T) {
	files := testFiles(t.TempDir())
	edited := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"AGENT_ARCHIVE_HOME=$X '/bin/agent-archive' _hook --harness claude # agent-archive lifecycle capture","statusMessage":"agent-archive lifecycle capture"}]}]}}`
	must(t, os.MkdirAll(filepath.Dir(files["claude"]), 0o700))
	must(t, os.WriteFile(files["claude"], []byte(edited), 0o600))
	others, err := OtherInstallations(files, Hook{}, "claude")
	must(t, err)
	if len(others) != 1 || others[0].Command == "" {
		t.Fatalf("others = %+v", others)
	}
	if _, found, err := PlanRemovalOf(files, Hook{}, "claude"); err != nil || found {
		t.Fatalf("removal planned: found=%v err=%v", found, err)
	}
}
