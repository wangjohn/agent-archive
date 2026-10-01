package platform

import (
	"path/filepath"
	"strconv"
)

// LocationDeps are the inputs of Locations that come from the running
// system, injected so this package stays free of exec and the file system.
// Every field is optional: a nil function means the system cannot say.
type LocationDeps struct {
	// DarwinUserTempDir is the per-user temporary directory macOS reports
	// (getconf DARWIN_USER_TEMP_DIR), "" when it cannot be read. It is
	// called only for Darwin and only by SnapshotRoot.
	DarwinUserTempDir func() string
	// ProcessTempDir is the process's own idea of the temporary directory
	// (os.TempDir), the last resort before /tmp on macOS. Nil skips it.
	ProcessTempDir func() string
	// ResolveSymlinks returns path with its symbolic links resolved (and
	// cleaned), as far as they can be. Nil leaves a path as given, cleaned.
	// It is called only by ProtectedFolders.
	ResolveSymlinks func(path string) string
	// UID is the user's ID. The macOS snapshot folder's name carries it,
	// which keeps users apart where the temporary directory is shared.
	UID int
	// AccountHome is the account's home directory from the user database,
	// not $HOME (which a shell or a sandbox may set to anything). The Linux
	// snapshot root defaults to a folder under it. "" when the system cannot
	// say; a relative path counts as that.
	AccountHome string
}

// Locations are where an operating system keeps what agent-archive looks
// for, for one user: computed from (OS, home, environment), so a test passes
// values and not mocks.
//
// A location is "" when it does not exist on the OS (the macOS desktop apps'
// folders on Linux) or when it cannot be known: an Unknown OS has no known
// locations, and an empty home has no folder under it. A caller must treat
// "" as "not there" and never join it into a path.
//
// Two values need the system and are methods that call LocationDeps only
// when asked, so building Locations never runs a program or touches the
// disk: ProtectedFolders and SnapshotRoot.
type Locations struct {
	// OS is the system these are the locations of.
	OS OS

	// CursorAppDir is Cursor's per-user application-data folder, the parent
	// of its User folder.
	//
	//   - Darwin: <home>/Library/Application Support/Cursor.
	//   - Linux: Cursor is a VS Code fork and keeps its data where VS Code
	//     does, under the XDG config home: $XDG_CONFIG_HOME/Cursor when that
	//     is an absolute path, else <home>/.config/Cursor. The XDG Base
	//     Directory specification says a relative $XDG_CONFIG_HOME is invalid
	//     and must be ignored, as is an empty one. The Linux layout is the VS
	//     Code convention and has not been confirmed on a real Cursor
	//     install.
	//   - Unknown: "". Cursor's data is not looked for on a system this
	//     program does not know, so Cursor there counts as not installed.
	CursorAppDir string
	// CursorStateDB is Cursor's chat database, state.vscdb, under
	// CursorAppDir; "" when CursorAppDir is.
	CursorStateDB string
	// CursorWorkspaceStorage is Cursor's folder of per-workspace state; ""
	// when CursorAppDir is.
	CursorWorkspaceStorage string

	// ClaudeDesktopScratch is where the Claude desktop app starts scratch
	// chats, and CodexDocuments where the Codex desktop app puts its dated
	// workspaces (<date>/<name>). Both are macOS desktop-app locations: ""
	// on any other system.
	ClaudeDesktopScratch string
	CodexDocuments       string

	// TempRoots are the temporary directories besides $TMPDIR: sessions in
	// one are skipped as temporary. macOS has /tmp (a link to /private/tmp)
	// and the per-user folders under /var/folders, both spellings of each;
	// Linux has /tmp and /var/tmp. An Unknown system gets every one of them:
	// treating a folder as temporary keeps its sessions out of an import, the
	// safer error. Do not modify the slice.
	TempRoots []string

	home   string
	getenv func(string) string
	deps   LocationDeps
}

// NewLocations is the locations for os under home, reading the environment
// through getenv (nil is an empty environment) and the system through deps.
func NewLocations(os OS, home string, getenv func(string) string, deps LocationDeps) Locations {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	l := Locations{
		OS: os, home: home, getenv: getenv, deps: deps,
		// Anything but a known system, Unknown included, skips every
		// temporary directory.
		TempRoots: []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/var/tmp"},
	}
	switch os {
	case Darwin:
		if home != "" {
			support := filepath.Join(home, "Library", "Application Support")
			l.CursorAppDir = filepath.Join(support, "Cursor")
			l.ClaudeDesktopScratch = filepath.Join(support, "Claude", "scratch-workspaces")
			l.CodexDocuments = filepath.Join(home, "Documents", "Codex")
		}
		l.TempRoots = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	case Linux:
		if dir := getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
			l.CursorAppDir = filepath.Join(dir, "Cursor")
		} else if home != "" {
			l.CursorAppDir = filepath.Join(home, ".config", "Cursor")
		}
		l.TempRoots = []string{"/tmp", "/var/tmp"}
	case Unknown:
	}
	if l.CursorAppDir != "" {
		l.CursorStateDB = filepath.Join(l.CursorAppDir, "User", "globalStorage", "state.vscdb")
		l.CursorWorkspaceStorage = filepath.Join(l.CursorAppDir, "User", "workspaceStorage")
	}
	return l
}

// ProtectedFolders are the folders whose contents macOS shows a privacy
// prompt (TCC) for before an app reads them: the home folder's Desktop,
// Documents, Downloads and Library; iCloud Drive (Library/Mobile Documents)
// and other apps' data (Library/Containers and Library/Group Containers),
// locations of their own inside Library; and the other volumes: removable
// and network ones under /Volumes, and the same data reached through
// /System/Volumes, /Network or /net. Both home as given and with its
// symlinks resolved (deps.ResolveSymlinks) are covered.
//
// TCC is macOS only: on any other system, Unknown included, nothing is
// protected and no folder is kept out unread (a ~/Documents on Linux is an
// ordinary folder), so it is nil. An Unknown system is not treated as a Mac
// here: the prompt cannot happen on it, and guessing one would make folders
// unreadable to the import for no reason.
func (l Locations) ProtectedFolders() []string {
	if l.OS != Darwin || l.home == "" {
		return nil
	}
	homes := []string{filepath.Clean(l.home)}
	if l.deps.ResolveSymlinks != nil {
		if resolved := l.deps.ResolveSymlinks(l.home); resolved != homes[0] {
			homes = append(homes, resolved)
		}
	}
	var out []string
	for _, home := range homes {
		for _, name := range []string{"Desktop", "Documents", "Downloads", "Library", filepath.Join("Library", "Mobile Documents"), filepath.Join("Library", "Containers"), filepath.Join("Library", "Group Containers")} {
			out = append(out, filepath.Join(home, name))
		}
	}
	return append(out, "/Volumes", "/System/Volumes", "/Network", "/net")
}

// fallbackTempDir is the temporary directory of last resort.
const fallbackTempDir = "/tmp"

// SnapshotDirName is the name of the per-user directory Cursor database
// copies go in, under macOS's temporary directory.
func SnapshotDirName(uid int) string { return "agent-archive-cursor-" + strconv.Itoa(uid) }

// The Linux snapshot root is <cache>/agent-archive/cursor-snapshots, where
// <cache> is the XDG cache home.
const (
	// CacheDirName is agent-archive's folder in the cache home. It holds the
	// CACHEDIR.TAG that tells backup tools to skip it.
	CacheDirName = "agent-archive"
	// linuxSnapshotDirName is the folder of database copies in CacheDirName.
	linuxSnapshotDirName = "cursor-snapshots"
)

// cacheHome is the XDG cache home on Linux: $XDG_CACHE_HOME when it is an
// absolute path (the XDG Base Directory specification says a relative one is
// invalid and must be ignored, as is an empty one), else ~/.cache under the
// account's home. It is "" when neither is known, and on any other system.
func (l Locations) cacheHome() string {
	if l.OS != Linux {
		return ""
	}
	if dir := l.getenv("XDG_CACHE_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if filepath.IsAbs(l.deps.AccountHome) {
		return filepath.Join(filepath.Clean(l.deps.AccountHome), ".cache")
	}
	return ""
}

// SnapshotCacheDir is agent-archive's folder in the XDG cache home on Linux
// (<cache>/agent-archive), the parent of SnapshotRoot and the place for a
// CACHEDIR.TAG. It is "" on any other system, and on Linux when the cache
// home is not known.
func (l Locations) SnapshotCacheDir() string {
	if cache := l.cacheHome(); cache != "" {
		return filepath.Join(cache, CacheDirName)
	}
	return ""
}

// SnapshotRoot is where copies of Cursor's database are made. A copy holds
// every Cursor chat, including those of projects that are not archived, so it
// stays out of the archive home, which may be backed up or synced.
//
// On macOS it is a directory of this user's own (SnapshotDirName) in the
// per-user temporary directory the system reports (deps.DarwinUserTempDir),
// whatever $TMPDIR says: it is under /var/folders, which Time Machine
// excludes, and every process of the user's agrees on it, whereas the
// LaunchAgent that runs the collector sets only AGENT_ARCHIVE_HOME (so
// launchd may leave TMPDIR unset) and a custom TMPDIR in a shell would put a
// copy of every chat elsewhere and split the sweep's root. Only if the system
// cannot say, it is an absolute $TMPDIR, then deps.ProcessTempDir, then /tmp.
// A relative $TMPDIR would put the copy of every chat relative to the working
// directory, wherever that is, so it is ignored, as if unset.
//
// On Linux it is <cache>/agent-archive/cursor-snapshots (SnapshotCacheDir),
// where <cache> is $XDG_CACHE_HOME or ~/.cache (~ is the account's home from
// the user database, not $HOME). It is not under /tmp: that is shared, so
// another local user can make a predictable name there first and keep a copy
// from ever being made; it is often a tmpfs, which would put a copy as large
// as Cursor's database in memory; and a job started by systemd has its own
// idea of $TMPDIR. $XDG_CACHE_HOME is one of the variables setup records in
// the collector's unit, so a shell and the collector agree on the root and the
// sweep of stale copies sees one directory. With no cache home and no account
// home there is no root ("").
//
// An Unknown system has no snapshot root: it is "", because Cursor's
// database is not looked for there (CursorStateDB is ""), so no copy is ever
// wanted, and guessing a shared temporary directory for one would be a
// privacy decision made without knowing the system. A caller must treat ""
// as "take no snapshot".
func (l Locations) SnapshotRoot() string {
	switch l.OS {
	case Linux:
		if dir := l.SnapshotCacheDir(); dir != "" {
			return filepath.Join(dir, linuxSnapshotDirName)
		}
		return ""
	case Darwin:
		return l.darwinSnapshotRoot()
	case Unknown:
		return ""
	}
	return ""
}

// darwinSnapshotRoot is SnapshotRoot on macOS.
func (l Locations) darwinSnapshotRoot() string {
	name := SnapshotDirName(l.deps.UID)
	if l.deps.DarwinUserTempDir != nil {
		if dir := l.deps.DarwinUserTempDir(); filepath.IsAbs(dir) {
			return filepath.Join(dir, name)
		}
	}
	if dir := l.getenv("TMPDIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, name)
	}
	if l.deps.ProcessTempDir != nil {
		if dir := l.deps.ProcessTempDir(); filepath.IsAbs(dir) {
			return filepath.Join(dir, name)
		}
	}
	return filepath.Join(fallbackTempDir, name)
}
