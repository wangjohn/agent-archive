package platform

import (
	"path/filepath"
	"slices"
	"testing"
)

// otherSystems are names of systems the program does not know, and no name
// at all, however they got here; each is converted to an OS at run time.
var otherSystems = []string{"freebsd", "windows", ""}

func envOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// Cursor's data folder, database and workspace storage on every system, from
// injected values, so each row runs on any host.
func TestCursorLocations(t *testing.T) {
	t.Parallel()
	const home = "/home/me"
	for _, tc := range []struct {
		name   string
		os     OS
		home   string
		env    map[string]string
		getenv func(string) string // used instead of env when set
		want   string              // CursorAppDir
	}{
		{"macOS", Darwin, home, nil, nil, "/home/me/Library/Application Support/Cursor"},
		{"macOS ignores XDG_CONFIG_HOME", Darwin, home, map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/home/me/Library/Application Support/Cursor"},
		{"Linux default", Linux, home, nil, nil, "/home/me/.config/Cursor"},
		{"Linux XDG_CONFIG_HOME", Linux, home, map[string]string{"XDG_CONFIG_HOME": "/xdg/config"}, nil, "/xdg/config/Cursor"},
		{"Linux XDG_CONFIG_HOME with trailing slash", Linux, home, map[string]string{"XDG_CONFIG_HOME": "/xdg/config/"}, nil, "/xdg/config/Cursor"},
		{"Linux empty XDG_CONFIG_HOME", Linux, home, map[string]string{"XDG_CONFIG_HOME": ""}, nil, "/home/me/.config/Cursor"},
		{"Linux relative XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux dot-relative XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "./cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux tilde XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "~/cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux without an environment", Linux, home, nil, func(string) string { return "" }, "/home/me/.config/Cursor"},
		{"Linux with a nil getenv", Linux, home, nil, nil, "/home/me/.config/Cursor"},
		// An unknown system has no Cursor location: it is not quietly given
		// the Linux (VS Code) layout, XDG_CONFIG_HOME or not.
		{"unknown system", Unknown, home, nil, nil, ""},
		{"unknown system ignores XDG_CONFIG_HOME", Unknown, home, map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, ""},
		// Without a home there is no folder under it; never a relative path.
		{"macOS without a home", Darwin, "", nil, nil, ""},
		{"Linux without a home", Linux, "", nil, nil, ""},
		{"Linux without a home still honors an absolute XDG_CONFIG_HOME", Linux, "", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/xdg/Cursor"},
	} {
		getenv := tc.getenv
		if getenv == nil && tc.env != nil {
			getenv = envOf(tc.env)
		}
		loc := NewLocations(tc.os, tc.home, getenv, LocationDeps{})
		if loc.CursorAppDir != tc.want {
			t.Errorf("%s: CursorAppDir = %q, want %q", tc.name, loc.CursorAppDir, tc.want)
		}
		wantDB, wantStorage := "", ""
		if tc.want != "" {
			wantDB = filepath.Join(tc.want, "User", "globalStorage", "state.vscdb")
			wantStorage = filepath.Join(tc.want, "User", "workspaceStorage")
		}
		if loc.CursorStateDB != wantDB {
			t.Errorf("%s: CursorStateDB = %q, want %q", tc.name, loc.CursorStateDB, wantDB)
		}
		if loc.CursorWorkspaceStorage != wantStorage {
			t.Errorf("%s: CursorWorkspaceStorage = %q, want %q", tc.name, loc.CursorWorkspaceStorage, wantStorage)
		}
		if loc.OS != tc.os {
			t.Errorf("%s: OS = %q, want %q", tc.name, loc.OS, tc.os)
		}
	}
}

// Any other name is as unknown as Unknown itself: no Cursor location, not
// the Linux one.
func TestOtherSystemsHaveNoCursorLocation(t *testing.T) {
	t.Parallel()
	for _, name := range otherSystems {
		loc := NewLocations(OS(name), "/home/me", envOf(map[string]string{"XDG_CONFIG_HOME": "/xdg"}), LocationDeps{})
		if loc.CursorAppDir != "" || loc.CursorStateDB != "" || loc.CursorWorkspaceStorage != "" {
			t.Errorf("%q: Cursor locations %q %q %q, want none", name, loc.CursorAppDir, loc.CursorStateDB, loc.CursorWorkspaceStorage)
		}
	}
}

// The systemd user unit directory is a Linux location under the home, and
// does not follow XDG_CONFIG_HOME (the user manager reads that only from its
// own environment).
func TestUserUnitDirIsLinuxOnly(t *testing.T) {
	t.Parallel()
	env := envOf(map[string]string{"XDG_CONFIG_HOME": "/xdg"})
	if got, want := NewLocations(Linux, "/home/me", env, LocationDeps{}).UserUnitDir, "/home/me/.config/systemd/user"; got != want {
		t.Errorf("Linux: UserUnitDir = %q, want %q", got, want)
	}
	if got := NewLocations(Linux, "", env, LocationDeps{}).UserUnitDir; got != "" {
		t.Errorf("Linux with no home: UserUnitDir = %q, want none", got)
	}
	for _, name := range append([]string{"darwin"}, otherSystems...) {
		if got := NewLocations(OS(name), "/home/me", env, LocationDeps{}).UserUnitDir; got != "" {
			t.Errorf("%q: UserUnitDir = %q, want none", name, got)
		}
	}
}

// The desktop apps' folders are macOS locations: absent (empty) everywhere
// else, an unknown system included.
func TestDesktopAppFoldersAreMacOnly(t *testing.T) {
	t.Parallel()
	mac := NewLocations(Darwin, "/Users/me", nil, LocationDeps{})
	if want := "/Users/me/Library/Application Support/Claude/scratch-workspaces"; mac.ClaudeDesktopScratch != want {
		t.Errorf("ClaudeDesktopScratch = %q, want %q", mac.ClaudeDesktopScratch, want)
	}
	if want := "/Users/me/Documents/Codex"; mac.CodexDocuments != want {
		t.Errorf("CodexDocuments = %q, want %q", mac.CodexDocuments, want)
	}
	for _, name := range append([]string{"linux", "unknown"}, otherSystems...) {
		system := OS(name)
		loc := NewLocations(system, "/home/me", nil, LocationDeps{})
		if loc.ClaudeDesktopScratch != "" || loc.CodexDocuments != "" {
			t.Errorf("%q: desktop app folders %q and %q, want none", system, loc.ClaudeDesktopScratch, loc.CodexDocuments)
		}
	}
	if loc := NewLocations(Darwin, "", nil, LocationDeps{}); loc.ClaudeDesktopScratch != "" || loc.CodexDocuments != "" {
		t.Errorf("no home: desktop app folders %q and %q, want none", loc.ClaudeDesktopScratch, loc.CodexDocuments)
	}
}

// The temporary directories backfill skips, per system. An unknown system gets
// every one of them: it errs toward treating a folder as temporary.
func TestTempRoots(t *testing.T) {
	t.Parallel()
	mac := []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	linux := []string{"/tmp", "/var/tmp"}
	unknownRoots := append(slices.Clone(mac), "/var/tmp")
	for system, want := range map[OS][]string{
		Darwin:  mac,
		Linux:   linux,
		Unknown: unknownRoots,
	} {
		got := NewLocations(system, "/h", nil, LocationDeps{}).TempRoots
		if !slices.Equal(got, want) {
			t.Errorf("%q: TempRoots = %v, want %v", system, got, want)
		}
	}
	for _, name := range otherSystems {
		if got := NewLocations(OS(name), "/h", nil, LocationDeps{}).TempRoots; !slices.Equal(got, unknownRoots) {
			t.Errorf("%q: TempRoots = %v, want %v", name, got, unknownRoots)
		}
	}
	// Every known system's roots are inside the unknown system's, so the
	// conservative choice can only skip more.
	unknown := NewLocations(Unknown, "/h", nil, LocationDeps{}).TempRoots
	for _, root := range slices.Concat(mac, linux) {
		if !slices.Contains(unknown, root) {
			t.Errorf("an unknown system does not skip %s", root)
		}
	}
}

// The macOS privacy-protected folders, for home as given and resolved; none
// anywhere else, and never guessed for an unknown system.
func TestProtectedFolders(t *testing.T) {
	t.Parallel()
	identity := func(p string) string { return p }
	mac := NewLocations(Darwin, "/Users/me", nil, LocationDeps{ResolveSymlinks: identity}).ProtectedFolders()
	want := []string{
		"/Users/me/Desktop", "/Users/me/Documents", "/Users/me/Downloads", "/Users/me/Library",
		"/Users/me/Library/Mobile Documents", "/Users/me/Library/Containers", "/Users/me/Library/Group Containers",
		"/Volumes", "/System/Volumes", "/Network", "/net",
	}
	if !slices.Equal(mac, want) {
		t.Errorf("macOS protected folders = %v, want %v", mac, want)
	}

	// A home that is a symlink covers both spellings, as given first.
	linked := NewLocations(Darwin, "/Users/me/", nil, LocationDeps{ResolveSymlinks: func(string) string { return "/private/Users/me" }}).ProtectedFolders()
	for _, folder := range []string{"/Users/me/Documents", "/private/Users/me/Documents", "/private/Users/me/Library/Group Containers", "/Volumes"} {
		if !slices.Contains(linked, folder) {
			t.Errorf("a linked home does not protect %s: %v", folder, linked)
		}
	}
	if len(linked) != 2*7+4 {
		t.Errorf("a linked home has %d protected folders, want %d", len(linked), 2*7+4)
	}

	// No resolver: home as given, cleaned.
	if got := NewLocations(Darwin, "/Users/me/", nil, LocationDeps{}).ProtectedFolders(); len(got) != len(want) || got[0] != "/Users/me/Desktop" {
		t.Errorf("without a resolver: %v", got)
	}

	for _, name := range append([]string{"linux", "unknown"}, otherSystems...) {
		system := OS(name)
		called := false
		deps := LocationDeps{ResolveSymlinks: func(p string) string { called = true; return p }}
		if got := NewLocations(system, "/home/me", nil, deps).ProtectedFolders(); got != nil {
			t.Errorf("%q: protected folders %v, want none", system, got)
		}
		if called {
			t.Errorf("%q: symlinks were resolved for nothing", system)
		}
	}
	if got := NewLocations(Darwin, "", nil, LocationDeps{}).ProtectedFolders(); got != nil {
		t.Errorf("no home: protected folders %v, want none", got)
	}
}

// The macOS snapshot root is the per-user temporary directory the system
// reports, whatever $TMPDIR says, so a collector started by launchd without
// TMPDIR, a hook run with it, and a shell with a custom one share one root;
// $TMPDIR only when the system can't say.
func TestSnapshotRoot(t *testing.T) {
	t.Parallel()
	const uid = 501
	name := "agent-archive-cursor-501"
	perUser := func() string { return "/var/folders/xy/abc/T" }
	none := func() string { return "" }
	process := func() string { return "/process/tmp" }
	for _, tc := range []struct {
		name    string
		os      OS
		tmpdir  string
		darwin  func() string
		process func() string
		want    string
	}{
		{"macOS, custom TMPDIR", Darwin, "/private/tmp/mine", perUser, process, "/var/folders/xy/abc/T"},
		{"macOS launched without TMPDIR", Darwin, "", perUser, process, "/var/folders/xy/abc/T"},
		{"macOS, getconf failed", Darwin, "/private/tmp/mine", none, process, "/private/tmp/mine"},
		{"macOS, getconf failed, no TMPDIR", Darwin, "", none, process, "/process/tmp"},
		{"macOS, no getconf at all", Darwin, "/private/tmp/mine", nil, process, "/private/tmp/mine"},
		{"macOS relative getconf answer is ignored", Darwin, "/private/tmp/mine", func() string { return "relative" }, process, "/private/tmp/mine"},
		{"macOS relative TMPDIR, getconf failed", Darwin, "tmp", none, process, "/process/tmp"},
		{"macOS relative TMPDIR, getconf answers", Darwin, "tmp", perUser, process, "/var/folders/xy/abc/T"},
		// A relative $TMPDIR is ignored as if unset: the snapshot root, a copy
		// of every chat, must not depend on the working directory.
		{"macOS dot-relative TMPDIR", Darwin, "./tmp", none, process, "/process/tmp"},
		{"macOS tilde TMPDIR", Darwin, "~/tmp", none, process, "/process/tmp"},
		{"macOS relative process temp dir falls back to /tmp", Darwin, "tmp", none, func() string { return "relative/tmp" }, "/tmp"},
		{"macOS with no process temp dir", Darwin, "", none, nil, "/tmp"},
		{"macOS absolute TMPDIR, getconf failed", Darwin, "/var/tmp/mine", none, process, "/var/tmp/mine"},
		// An unknown system has no snapshot root, whatever it is given.
		{"unknown system", Unknown, "/tmp/mine", perUser, process, ""},
	} {
		deps := LocationDeps{DarwinUserTempDir: tc.darwin, ProcessTempDir: tc.process, UID: uid}
		got := NewLocations(tc.os, "/h", envOf(map[string]string{"TMPDIR": tc.tmpdir}), deps).SnapshotRoot()
		want := ""
		if tc.want != "" {
			want = filepath.Join(tc.want, name)
		}
		if got != want {
			t.Errorf("%s: %q, want %q", tc.name, got, want)
		}
	}
}

// The Linux snapshot root is under the XDG cache home, never a temporary
// directory: $XDG_CACHE_HOME when absolute, else ~/.cache of the account's
// home from the user database (not the $HOME the shell says), and none when
// neither is known.
func TestLinuxSnapshotRootIsUnderTheCacheHome(t *testing.T) {
	t.Parallel()
	deps := LocationDeps{
		DarwinUserTempDir: func() string { return "/var/folders/xy/abc/T" },
		ProcessTempDir:    func() string { return "/process/tmp" },
		UID:               1000,
		AccountHome:       "/home/ada",
	}
	const defaultRoot, defaultDir = "/home/ada/.cache/agent-archive/cursor-snapshots", "/home/ada/.cache/agent-archive"
	const srvRoot, srvDir = "/srv/cache/agent-archive/cursor-snapshots", "/srv/cache/agent-archive"
	for _, tc := range []struct {
		name string
		env  map[string]string
		deps LocationDeps
		home string
		root string
		dir  string
	}{
		{"default", nil, deps, "/home/ada", defaultRoot, defaultDir},
		{"absolute XDG_CACHE_HOME", map[string]string{"XDG_CACHE_HOME": "/srv/cache"}, deps, "/home/ada", srvRoot, srvDir},
		{"XDG_CACHE_HOME is cleaned", map[string]string{"XDG_CACHE_HOME": "/srv/cache/"}, deps, "/home/ada", srvRoot, srvDir},
		{"empty XDG_CACHE_HOME is unset", map[string]string{"XDG_CACHE_HOME": ""}, deps, "/home/ada", defaultRoot, defaultDir},
		{"relative XDG_CACHE_HOME is ignored", map[string]string{"XDG_CACHE_HOME": "cache"}, deps, "/home/ada", defaultRoot, defaultDir},
		{"tilde XDG_CACHE_HOME is ignored", map[string]string{"XDG_CACHE_HOME": "~/cache"}, deps, "/home/ada", defaultRoot, defaultDir},
		// TMPDIR and the temporary directories play no part.
		{"TMPDIR is ignored", map[string]string{"TMPDIR": "/tmp/mine"}, deps, "/home/ada", defaultRoot, defaultDir},
		// The home NewLocations is given ($HOME) is not the account's home.
		{"the account's home, not $HOME", nil, deps, "/sandbox/home", defaultRoot, defaultDir},
		{"no account home uses the absolute cache home", map[string]string{"XDG_CACHE_HOME": "/srv/cache"}, LocationDeps{}, "/home/ada", srvRoot, srvDir},
		{"no account home and no cache home", nil, LocationDeps{}, "/home/ada", "", ""},
		{"relative account home", nil, LocationDeps{AccountHome: "home/ada"}, "/home/ada", "", ""},
	} {
		loc := NewLocations(Linux, tc.home, envOf(tc.env), tc.deps)
		if got := loc.SnapshotRoot(); got != tc.root {
			t.Errorf("%s: root %q, want %q", tc.name, got, tc.root)
		}
		if got := loc.SnapshotCacheDir(); got != tc.dir {
			t.Errorf("%s: cache dir %q, want %q", tc.name, got, tc.dir)
		}
	}
}

// Only Linux has an agent-archive cache folder; the macOS root stays in the
// per-user temporary directory whatever XDG_CACHE_HOME says.
func TestOnlyLinuxHasACacheDirForSnapshots(t *testing.T) {
	t.Parallel()
	deps := LocationDeps{DarwinUserTempDir: func() string { return "/var/folders/xy/abc/T" }, UID: 501, AccountHome: "/Users/ada"}
	env := envOf(map[string]string{"XDG_CACHE_HOME": "/srv/cache"})
	for _, system := range []OS{Darwin, Unknown} {
		if got := NewLocations(system, "/Users/ada", env, deps).SnapshotCacheDir(); got != "" {
			t.Errorf("%s: cache dir %q, want none", system, got)
		}
	}
	if got, want := NewLocations(Darwin, "/Users/ada", env, deps).SnapshotRoot(), "/var/folders/xy/abc/T/agent-archive-cursor-501"; got != want {
		t.Errorf("macOS root %q, want %q", got, want)
	}
}

func TestOtherSystemsHaveNoSnapshotRoot(t *testing.T) {
	t.Parallel()
	deps := LocationDeps{DarwinUserTempDir: func() string { return "/var/folders/T" }, ProcessTempDir: func() string { return "/process/tmp" }}
	for _, name := range otherSystems {
		if got := NewLocations(OS(name), "/h", envOf(map[string]string{"TMPDIR": "/tmp/mine"}), deps).SnapshotRoot(); got != "" {
			t.Errorf("%q: snapshot root %q, want none", name, got)
		}
	}
}

// The system's per-user temporary directory is asked for only by macOS and
// only when the snapshot root is wanted, so building Locations, or asking for
// anything else, never runs getconf.
func TestSystemDependenciesAreCalledOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	calls := 0
	deps := LocationDeps{
		DarwinUserTempDir: func() string { calls++; return "/var/folders/T" },
		ResolveSymlinks:   func(p string) string { calls++; return p },
	}
	for _, system := range []OS{Darwin, Linux, Unknown} {
		loc := NewLocations(system, "/h", nil, deps)
		_ = loc.TempRoots
		_ = loc.CursorStateDB
		_ = loc.ClaudeDesktopScratch
	}
	if calls != 0 {
		t.Fatalf("building Locations called the system %d times", calls)
	}
	NewLocations(Linux, "/h", nil, deps).SnapshotRoot()
	NewLocations(Unknown, "/h", nil, deps).SnapshotRoot()
	if calls != 0 {
		t.Fatalf("a non-macOS snapshot root called getconf %d times", calls)
	}
	NewLocations(Darwin, "/h", nil, deps).SnapshotRoot()
	if calls != 1 {
		t.Fatalf("the macOS snapshot root called getconf %d times, want 1", calls)
	}
}

func TestSnapshotDirNameCarriesTheUserID(t *testing.T) {
	t.Parallel()
	if got := SnapshotDirName(501); got != "agent-archive-cursor-501" {
		t.Errorf("SnapshotDirName(501) = %q", got)
	}
	if SnapshotDirName(0) == SnapshotDirName(1) {
		t.Error("two users share a snapshot directory name")
	}
}
