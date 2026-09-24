// Package backfill finds the Claude Code, Codex, and Cursor sessions already
// on this Mac and plans importing them into the archive (see
// docs/design/backfill.md). Planning is read-only: it lists the
// apps' native stores, runs each app's adapter over every transcript it may
// import, and keeps only counts, times, and sizes. File system access, the
// home directory, and the clock come from an injected Environment, so tests
// run over a constructed tree.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// SkipReason says why a found session is not imported. Every session found is
// either imported (an empty reason) or has exactly one reason; when several
// apply, the earliest in skipOrder wins.
type SkipReason string

const (
	SkipAlreadyArchived       SkipReason = "already_archived"
	SkipDuplicateSession      SkipReason = "duplicate_session"
	SkipRegisteredNotAdmitted SkipReason = "registered_not_admitted"
	SkipRemovedByUndo         SkipReason = "removed_by_undo"
	SkipRemovedByRetention    SkipReason = "removed_by_retention"
	SkipFilteredOut           SkipReason = "filtered_out"
	SkipExcludedProject       SkipReason = "excluded_project"
	SkipHomeDirectory         SkipReason = "home_directory"
	SkipAboveHome             SkipReason = "above_home"
	SkipTemporaryDirectory    SkipReason = "temporary_directory"
	SkipProjectUnknown        SkipReason = "project_unknown"
	SkipWorktreeUnresolved    SkipReason = "worktree_unresolved"
	SkipIdentityMismatch      SkipReason = "identity_mismatch"
	SkipEmpty                 SkipReason = "empty"
	SkipUnsafeFormat          SkipReason = "unsafe_format"
	SkipTooLarge              SkipReason = "too_large"
	SkipStartUnknown          SkipReason = "start_unknown"
	SkipStartInFuture         SkipReason = "start_in_future"
)

// skipOrder is the spec's precedence: the first applicable reason wins.
var skipOrder = []SkipReason{
	SkipAlreadyArchived, SkipDuplicateSession, SkipRegisteredNotAdmitted, SkipRemovedByUndo, SkipRemovedByRetention,
	SkipFilteredOut, SkipExcludedProject, SkipHomeDirectory, SkipAboveHome, SkipTemporaryDirectory,
	SkipProjectUnknown, SkipWorktreeUnresolved, SkipIdentityMismatch,
	SkipEmpty, SkipUnsafeFormat, SkipTooLarge, SkipStartUnknown, SkipStartInFuture,
}

// ArchiveState is what backfill needs to know about the local archive. The
// CLI implements it over collector.LocalStore and config.Config.
type ArchiveState interface {
	// Classify returns "" when the native session is not in the archive, or
	// one of the skip codes already_archived, registered_not_admitted,
	// removed_by_retention, removed_by_undo.
	Classify(harness, nativeSessionID string) (SkipReason, error)
}

// ProjectKind is what a resolved project root is, which decides where the
// plan lists it.
type ProjectKind string

const (
	// ProjectKindRepository is a git repository, including one a worktree
	// folded into.
	ProjectKindRepository ProjectKind = "repository"
	// ProjectKindScratch is a desktop app's workspace folder: Claude
	// desktop's scratch-workspaces or Codex desktop's ~/Documents/Codex.
	ProjectKindScratch ProjectKind = "scratch"
	// ProjectKindTemporary is a temporary directory, only with IncludeTemp.
	ProjectKindTemporary ProjectKind = "temporary"
	// ProjectKindHome is the home directory, only with IncludeHome.
	ProjectKindHome ProjectKind = "home"
	// ProjectKindDirectory is any other folder.
	ProjectKindDirectory ProjectKind = "directory"
)

// Candidate is one session found on disk and what the plan decided for it.
// It holds paths and native IDs for the import step; nothing here is ever
// printed.
type Candidate struct {
	Harness         string
	NativeSessionID string
	// TranscriptPath is the session's transcript file. A Cursor chat found
	// only in Cursor's database has none: its SourceKind is
	// archive.SourceKindCursorSQLite and SourceKey its chat ID.
	TranscriptPath string
	SourceKind     archive.SourceKind
	SourceKey      string
	// ProjectRoot is where the session is imported, in the spelling it is
	// registered under: a configured project's own spelling, or the resolved
	// path. It is set for a skip that has a would-be root (home, temporary,
	// excluded) and empty when no project could be determined.
	ProjectRoot string
	ProjectKind ProjectKind
	// ProjectIncluded is set when the root is a configured, included
	// project; otherwise importing adds it.
	ProjectIncluded bool
	// ProjectExists is whether the root folder still exists.
	ProjectExists   bool
	StartedAt       time.Time
	StartedAtSource archive.StartedAtSource
	Bytes           int64
	// Subagents are the readable Claude Code subagent transcripts of an
	// imported parent.
	Subagents []Subagent
	// SubagentsSkipped counts the parent's subagent transcripts that are too
	// large, that the filter refuses, or that the collector would not
	// register (collector.CheckImportedSubagent); they are not imported.
	SubagentsSkipped int
	Skip             SkipReason
}

// Subagent is one subagent transcript belonging to an imported parent.
type Subagent struct {
	Path string
	// AgentID is the <id> of agent-<id>.jsonl.
	AgentID string
	Bytes   int64
}

// Filters narrows a backfill run.
type Filters struct {
	Harnesses []string
	Projects  []string
	// Since and Until are local dates, YYYY-MM-DD, compared inclusively with
	// the session's start.
	Since, Until   string
	IncludeHome    bool
	IncludeTemp    bool
	IncludeRemoved bool
}

// Active reports whether any filter that can exclude a session is set; only
// then does the plan report filtered_out.
func (f Filters) Active() bool {
	return len(f.Harnesses) > 0 || len(f.Projects) > 0 || f.Since != "" || f.Until != ""
}

const dateLayout = "2006-01-02"

// Validate checks the filters' values before any file is read.
func (f Filters) Validate() error {
	for _, h := range f.Harnesses {
		if canonicalHarness(h) == "" {
			return fmt.Errorf("--harness must be claude, codex, or cursor, not %q", h)
		}
	}
	for _, p := range f.Projects {
		if p == "" {
			return errors.New("--project needs a directory")
		}
	}
	var since, until time.Time
	var err error
	if f.Since != "" {
		if since, err = time.Parse(dateLayout, f.Since); err != nil {
			return fmt.Errorf("--since must be a date like 2026-09-01, not %q", f.Since)
		}
	}
	if f.Until != "" {
		if until, err = time.Parse(dateLayout, f.Until); err != nil {
			return fmt.Errorf("--until must be a date like 2026-09-01, not %q", f.Until)
		}
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return errors.New("--until is before --since")
	}
	return nil
}

// canonicalHarness returns the harness name the archive uses, or "" for an
// unknown one.
func canonicalHarness(name string) string {
	switch name {
	case "claude", "claude-code":
		return "claude"
	case "codex", "cursor":
		return name
	}
	return ""
}

// Environment is everything BuildPlan reads from the machine. A nil function
// uses the real file system. The one exception is the adapter pass: it goes
// through collector.FilterTranscriptFile, the collector's own filter, which
// reads transcripts from the real file system whatever is injected here.
type Environment struct {
	// Home is the user's home directory, where the apps keep their stores.
	Home string
	// TempDirs are the temporary directories (rule 6 of project resolution).
	// Nil means DefaultTempDirs; the CLI adds $TMPDIR.
	TempDirs []string
	Now      func() time.Time

	Stat         func(string) (fs.FileInfo, error)
	Lstat        func(string) (fs.FileInfo, error)
	ReadDir      func(string) ([]fs.DirEntry, error)
	ReadFile     func(string) ([]byte, error)
	Open         func(string) (io.ReadCloser, error)
	EvalSymlinks func(string) (string, error)
	// FileCreated returns a file's birth time, Cursor's start time. The
	// default reads it from the file system where it is recorded (macOS);
	// where it is not, or it fails, the modification time is used.
	FileCreated func(string) (time.Time, error)
	// CursorDatabase lists the chats in Cursor's database (composerData
	// entries with messages, not drafts, and not subagents) and reads them.
	// Nil means not checked; the CLI uses CursorDatabaseReader. Undo reads
	// the database under Home instead (see resumedSinceImport).
	CursorDatabase func(ctx context.Context) (CursorDatabaseResult, error)
	// Workers overrides the filter worker count; zero uses defaultWorkers.
	Workers int
}

// DefaultTempDirs are the temporary directories on macOS besides $TMPDIR.
var DefaultTempDirs = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}

func (e Environment) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Environment) stat(path string) (fs.FileInfo, error) {
	if e.Stat != nil {
		return e.Stat(path)
	}
	return os.Stat(path)
}

func (e Environment) lstat(path string) (fs.FileInfo, error) {
	if e.Lstat != nil {
		return e.Lstat(path)
	}
	return os.Lstat(path)
}

func (e Environment) readDir(path string) ([]fs.DirEntry, error) {
	if e.ReadDir != nil {
		return e.ReadDir(path)
	}
	return os.ReadDir(path)
}

func (e Environment) readFile(path string) ([]byte, error) {
	if e.ReadFile != nil {
		return e.ReadFile(path)
	}
	return os.ReadFile(path)
}

func (e Environment) open(path string) (io.ReadCloser, error) {
	if e.Open != nil {
		return e.Open(path)
	}
	return os.Open(path)
}

func (e Environment) evalSymlinks(path string) (string, error) {
	if e.EvalSymlinks != nil {
		return e.EvalSymlinks(path)
	}
	return filepath.EvalSymlinks(path)
}

// fileCreated is a file's birth time, falling back to its modification time.
func (e Environment) fileCreated(path string) (time.Time, error) {
	var created time.Time
	var err error
	if e.FileCreated != nil {
		created, err = e.FileCreated(path)
	} else {
		created, err = fileBirthTime(path)
	}
	if err == nil && !created.IsZero() {
		return created, nil
	}
	info, statErr := e.lstat(path)
	if statErr != nil {
		if err == nil {
			err = statErr
		}
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func (e Environment) tempDirs() []string {
	if e.TempDirs != nil {
		return e.TempDirs
	}
	return DefaultTempDirs
}

// exists reports whether path exists.
func (e Environment) exists(path string) bool {
	_, err := e.stat(path)
	return err == nil
}

// resolved returns path cleaned with symlinks resolved, as hooks match
// configured projects. A path that no longer exists has its deepest existing
// ancestor resolved and the rest appended, so a missing folder under a
// symlinked parent gets the same spelling as when it existed.
func (e Environment) resolved(path string) string {
	path = filepath.Clean(path)
	if r, err := e.evalSymlinks(path); err == nil {
		return r
	}
	for ancestor := filepath.Dir(path); ; ancestor = filepath.Dir(ancestor) {
		if r, err := e.evalSymlinks(ancestor); err == nil {
			rel, err := filepath.Rel(ancestor, path)
			if err != nil {
				return path
			}
			return filepath.Join(r, rel)
		}
		if filepath.Dir(ancestor) == ancestor {
			return path
		}
	}
}
