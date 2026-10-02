package backfill

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sort"
	"syscall"
	"time"

	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// transcript is one native transcript file found on disk, before resolution.
type transcript struct {
	sourcePriority int
	harness        harness
	path           string
	size           int64
	// nativeID is the ID the session is registered under: the Claude Code
	// file stem, Codex's session_meta.payload.id, or the Cursor chat folder.
	nativeID string
	// cursorSlug is the Cursor project folder the chat was found in.
	cursorSlug string
	// cwd is the working directory the transcript records (Claude Code and
	// Codex).
	cwd string
	// identityMismatch is set when the transcript's own IDs disagree.
	identityMismatch bool
	// metaStart is Codex's session_meta timestamp.
	metaStart time.Time
}

// unreadable records what discovery could not list. Paths are never kept.
type unreadable struct {
	// folders counts folders inside an app's store.
	folders int
	// stores are the apps whose store root could not be listed, so none, or
	// for Codex's archived_sessions only the archived, of their sessions were
	// found.
	stores map[string]bool
	// codexArchivedOnly is set when Codex's archived_sessions could not be
	// listed but its sessions folder could.
	codexArchivedOnly bool
	// cursorIncomplete is set when any part of Cursor's transcript store
	// could not be listed, so a chat's transcript may exist unseen.
	cursorIncomplete bool
}

// listDir lists a folder inside an app's store. A missing folder, or a path
// that is not a folder, is empty; one that cannot be read is counted and
// treated as empty.
func listDir(env Environment, dir string, u *unreadable) []dirEntry {
	entries, ok := tryList(env, dir)
	if !ok {
		u.folders++
	}
	return entries
}

// listStore lists the root of app's store, recording app when it cannot be
// read: then none of the sessions below it are found.
func listStore(env Environment, dir, app string, u *unreadable) []dirEntry {
	entries, ok := tryList(env, dir)
	if !ok {
		u.stores[app] = true
	}
	return entries
}

// tryList lists dir; ok is false only when it exists as a folder and cannot
// be read.
func tryList(env Environment, dir string) ([]dirEntry, bool) {
	entries, err := readDirIfExists(env, dir)
	if err != nil {
		return nil, errors.Is(err, syscall.ENOTDIR)
	}
	return entries, true
}

// readDirIfExists lists dir, treating a missing directory as empty.
func readDirIfExists(env Environment, dir string) ([]dirEntry, error) {
	entries, err := env.readDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, dirEntry{name: e.Name(), dir: e.IsDir(), regular: e.Type().IsRegular()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

type dirEntry struct {
	name    string
	dir     bool
	regular bool
}

// fileSize stats a regular file without following symlinks; ok is false when
// it is gone or is not one. A symlinked transcript is skipped: discovery
// never follows a link out of an app's store.
func fileSize(env Environment, path string) (int64, bool) {
	info, err := env.lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	return info.Size(), true
}

// Import compatibility header bounds are distinct from native preview framing.
const (
	headLineLimit = 1 << 20
	headScanLimit = 8 << 20
)

// discoveryFiles adapts the read-only host port without exposing archive state.
type discoveryFiles struct{ env Environment }

func (d discoveryFiles) ReadDir(path string) ([]fs.DirEntry, error) { return d.env.readDir(path) }
func (d discoveryFiles) Lstat(path string) (fs.FileInfo, error)     { return d.env.lstat(path) }
func (d discoveryFiles) Open(path string) (io.ReadCloser, error)    { return d.env.open(path) }

func enumerateDiscovery(ctx context.Context, env Environment, purpose agentapi.DiscoveryPurpose, emit func(agentapi.DiscoveryCandidate) error) (unread unreadable, err error) {
	unread.stores = map[string]bool{}
	if env.Discovery == nil {
		return unread, fmt.Errorf("native discovery lookup required")
	}
	for _, name := range env.Discovery.DiscoveryAgents() {
		provider, _ := env.Discovery.LookupDiscovery(name)
		dirs := env.nativeDirectories(name)
		report, e := provider.Discover(ctx, agentapi.DiscoveryRequest{Purpose: purpose, Stage: agentapi.DiscoveryIdentities, Locations: agentapi.NativeLocations{UserHome: env.Home, Directories: dirs}, Files: discoveryFiles{env}, HeaderBytes: headScanLimit, RecordBytes: headLineLimit}, emit)
		if e != nil {
			return unread, e
		}
		unread.folders += report.UnreadableFolders
		if report.StoreUnreadable {
			unread.stores[name] = true
		}
		if name == "codex" {
			unread.codexArchivedOnly = report.HistoricalOnly
		}
		if name == "cursor" {
			unread.cursorIncomplete = report.Incomplete
		}
	}
	return unread, nil
}
