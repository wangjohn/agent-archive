package rolloutcatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const thread = "11111111-1111-4111-8111-111111111111"
const revision = "22222222-2222-4222-8222-222222222222"

func fixture(t *testing.T, home, store, rid, tid string, extra map[string]any, body string) string {
	t.Helper()
	dir := filepath.Join(home, store)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"id": tid, "cwd": "/synthetic/project", "timestamp": "2026-10-01T12:00:00Z", "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
	for k, v := range extra {
		meta[k] = v
	}
	raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 0, "payload": meta})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+rid+".jsonl")
	if err := os.WriteFile(path, append(append(raw, '\n'), []byte(body)...), 0600); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
func catalogSet(t *testing.T, c *Catalog, id string) agentapi.CodexRolloutSet {
	t.Helper()
	s, err := c.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestInventoryAcrossApprovedHomesAndNestedArchives(t *testing.T) {
	parent := t.TempDir()
	a := filepath.Join(parent, "custom home 日本語")
	b := filepath.Join(parent, "other home")
	if err := os.MkdirAll(b, 0700); err != nil {
		t.Fatal(err)
	}
	original := fixture(t, a, "sessions/2026/10/01", thread, thread, nil, "{\"ordinal\":1}\n")
	archived := fixture(t, b, "archived_sessions/nested/deeper", revision, thread, map[string]any{"history_base": map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": 10}}, "{\"ordinal\":2}\n")
	c := New([]string{a, b}, Limits{})
	if c.Counters().Headers != 0 {
		t.Fatal("constructor performed native reads")
	}
	s := catalogSet(t, c, thread)
	if !s.Complete || len(s.Candidates) != 2 || s.Current != nil {
		t.Fatalf("inventory %#v issues %#v", s, c.Issues())
	}
	if err := c.Check(t.Context(), thread, s.Revision); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{original, archived} {
		f, err := transcriptio.Open(c.Files(), path, transcriptio.OpenPolicy{RejectSymlinks: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	before := c.Counters()
	for range 50 {
		_ = catalogSet(t, c, thread)
		if err := c.Check(t.Context(), thread, s.Revision); err != nil {
			t.Fatal(err)
		}
	}
	after := c.Counters()
	if after.Headers != before.Headers || after.Directories != before.Directories || after.NativeQueries != before.NativeQueries {
		t.Fatalf("repeated thread enumeration: before=%#v after=%#v", before, after)
	}
}
func TestDuplicatePrefixAgreementAndConflicts(t *testing.T) {
	for _, scenario := range []string{"identical", "append", "body_conflict", "ownership_conflict"} {
		t.Run(scenario, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			one := fixture(t, a, "sessions", thread, thread, nil, "{\"ordinal\":1}\n")
			body := "{\"ordinal\":1}\n"
			extra := map[string]any{}
			if scenario == "append" {
				body += "{\"ordinal\":2}\n"
			}
			if scenario == "body_conflict" {
				body = "{\"ordinal\":9}\n"
			}
			if scenario == "ownership_conflict" {
				extra["cwd"] = "/other/project"
			}
			two := fixture(t, b, "archived_sessions", thread, thread, extra, body)
			c := New([]string{a, b}, Limits{})
			s := catalogSet(t, c, thread)
			refs, err := c.Rollout(t.Context(), thread)
			if err != nil {
				t.Fatal(err)
			}
			conflict := strings.Contains(scenario, "conflict")
			if conflict {
				if s.Complete || len(refs) != 2 {
					t.Fatalf("conflict lost: %#v %#v", s, refs)
				}
			} else {
				if !s.Complete || len(refs) != 1 {
					t.Fatalf("duplicates unresolved %#v %#v", s, refs)
				}
				if scenario == "append" && refs[0].Path != two {
					t.Fatal("longer observed prefix not selected")
				}
				if err := c.Check(t.Context(), thread, s.Revision); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(one)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-3] = '9'
				if err := os.WriteFile(one, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.Check(t.Context(), thread, s.Revision); agentapi.Failure(err) != agentapi.Changed {
					t.Fatalf("prefix rewrite accepted: %v", err)
				}
			}
		})
	}
}
func TestIncompleteInventoryNeverClaimsComplete(t *testing.T) {
	for _, scenario := range []string{"unknown_mode", "malformed", "entry_budget", "header_budget", "cancelled", "missing_root", "symlink_store"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			extra := map[string]any{}
			if scenario == "unknown_mode" {
				extra["history_mode"] = "future"
			}
			path := fixture(t, home, "sessions", thread, thread, extra, "")
			limits := Limits{}
			if scenario == "malformed" {
				if err := os.WriteFile(path, []byte("{bad}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "entry_budget" {
				limits.Entries = 1
				fixture(t, home, "sessions", revision, thread, nil, "")
			}
			if scenario == "header_budget" {
				limits.HeaderBytes = 8
			}
			if scenario == "missing_root" {
				home = filepath.Join(home, "missing")
			}
			if scenario == "symlink_store" {
				if err := os.Symlink(filepath.Dir(path), filepath.Join(home, "archived_sessions")); err != nil {
					t.Fatal(err)
				}
			}
			c := New([]string{home}, limits)
			ctx := t.Context()
			if scenario == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			s, err := c.Thread(ctx, thread)
			if scenario == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Complete || s.Current != nil || len(c.Issues()) == 0 {
				t.Fatalf("uncertainty claimed complete %#v %#v", s, c.Issues())
			}
		})
	}
}
func TestEpochFencesMembershipAndHeadersWhileAllowingAppend(t *testing.T) {
	for _, scenario := range []string{"append", "header_rewrite", "new_nested_file", "new_store", "replace", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			path := fixture(t, home, "sessions/nested", thread, thread, nil, "{\"ordinal\":1}\n")
			c := New([]string{home}, Limits{})
			s := catalogSet(t, c, thread)
			ctx := t.Context()
			switch scenario {
			case "append":
				f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if e != nil {
					t.Fatal(e)
				}
				_, e = f.WriteString("{\"ordinal\":2}\n")
				if e != nil {
					t.Fatal(e)
				}
				_ = f.Close()
			case "header_rewrite":
				fixture(t, home, "sessions/nested", thread, thread, map[string]any{"cwd": "/changed"}, "{\"ordinal\":1}\n")
			case "new_nested_file":
				fixture(t, home, "sessions/nested", revision, thread, nil, "")
			case "new_store":
				fixture(t, home, "archived_sessions", revision, thread, nil, "")
			case "replace":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				fixture(t, home, "sessions/nested", thread, thread, nil, "{\"ordinal\":1}\n")
			case "cancel":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err := c.Check(ctx, thread, s.Revision)
			if scenario == "append" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("changed epoch accepted")
			}
		})
	}
}
func TestApprovedOpenersRejectEscapesAndSymlinkComponents(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	path := fixture(t, home, "sessions", thread, thread, nil, "")
	foreign := fixture(t, outside, "sessions", revision, thread, nil, "")
	c := New([]string{home}, Limits{})
	if err := os.Symlink(filepath.Dir(foreign), filepath.Join(home, "sessions", "link")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{foreign, filepath.Join(home, "sessions", "link", filepath.Base(foreign)), filepath.Join(home, "sessions", "..", "secret.jsonl")} {
		if f, err := c.Files().OpenRegular(bad); err == nil {
			_ = f.Close()
			t.Fatalf("escaped root: %s", bad)
		}
	}
	if f, err := c.Files().OpenRegular(path); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
}
func nativeDB(t *testing.T, home, path, schema string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO threads(id,rollout_path) VALUES(?,?)", thread, path); err != nil {
		t.Fatal(err)
	}
}
func TestNativeCurrentIsIndexedReadOnlyValidatedHint(t *testing.T) {
	for _, scenario := range []string{"current", "stale", "outside", "schema", "unindexed", "wal", "lock", "db_absent"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				home := t.TempDir()
				one := fixture(t, home, "sessions", thread, thread, nil, "")
				two := fixture(t, home, "archived_sessions", revision, thread, nil, "")
				locator := two
				schema := "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)"
				if scenario == "stale" {
					locator = filepath.Join(home, "sessions", "missing.jsonl")
				}
				if scenario == "outside" {
					locator = fixture(t, t.TempDir(), "sessions", revision, thread, nil, "")
				}
				if scenario == "schema" {
					schema = "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path BLOB)"
				}
				if scenario == "unindexed" {
					schema = "CREATE TABLE threads(id TEXT,rollout_path TEXT)"
				}
				if scenario != "db_absent" {
					nativeDB(t, home, locator, schema)
				}
				var locked *sql.DB
				if scenario == "wal" {
					if err := os.WriteFile(filepath.Join(home, "state_5.sqlite-wal"), []byte("synthetic WAL"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "lock" {
					var err error
					locked, err = sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = locked.Close() }()
					if _, err := locked.Exec("BEGIN EXCLUSIVE; UPDATE threads SET rollout_path = ? WHERE id = ?", one, thread); err != nil {
						t.Fatal(err)
					}
					defer func() { _, _ = locked.Exec("ROLLBACK") }()
				}
				c := New([]string{home}, Limits{})
				s := catalogSet(t, c, thread)
				if !s.Complete {
					t.Fatalf("optional native failure invalidated file census: %#v", c.Issues())
				}
				wantCurrent := scenario == "current"
				if wantCurrent {
					if s.Current == nil || s.Current.Path != two {
						t.Fatalf("current missing: %#v issues %#v", s, c.Issues())
					}
				} else if s.Current != nil {
					t.Fatalf("unvalidated current selected %#v", s.Current)
				}
				if err := c.Check(t.Context(), thread, s.Revision); err != nil {
					t.Fatalf("settled unavailable-current fallback check: %v", err)
				}
				if wantCurrent {
					db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					_, err = db.Exec("UPDATE threads SET rollout_path = ? WHERE id = ?", one, thread)
					_ = db.Close()
					if err != nil {
						t.Fatal(err)
					}
					if err := c.Check(t.Context(), thread, s.Revision); err == nil {
						t.Fatal("native current selection change ignored")
					}
				}
			})
		})
	}
}

func TestSharedEpochCostsStayBoundedAcrossManyThreads(t *testing.T) {
	home := t.TempDir()
	for i := 1; i <= 100; i++ {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		fixture(t, home, "sessions/nested", id, id, nil, "")
	}
	c := New([]string{home}, Limits{})
	for i := 1; i <= 100; i++ {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		s := catalogSet(t, c, id)
		if !s.Complete || len(s.Candidates) != 1 {
			t.Fatal("inventory incomplete")
		}
		if err := c.Check(t.Context(), id, s.Revision); err != nil {
			t.Fatal(err)
		}
	}
	n := c.Counters()
	if n.Headers != 100 || n.Directories != 2 || n.NativeQueries != 0 || n.Checks != 100 || n.CheckBytes != 0 || n.CheckOperations > 11000 {
		t.Fatalf("shared evidence work unbounded %#v", n)
	}
}
func TestPrefixAndValidationBudgetsFailClosed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	fixture(t, a, "sessions", thread, thread, nil, "{\"ordinal\":1}\n")
	fixture(t, b, "archived_sessions", thread, thread, nil, "{\"ordinal\":1}\n")
	for _, limits := range []Limits{{PrefixBytes: 1}, {Records: 1}} {
		c := New([]string{a, b}, limits)
		s := catalogSet(t, c, thread)
		if s.Complete || s.Current != nil {
			t.Fatalf("duplicate budget claimed complete %#v", s)
		}
	}
	c := New([]string{a}, Limits{CheckOperations: 1})
	s := catalogSet(t, c, thread)
	if err := c.Check(t.Context(), thread, s.Revision); agentapi.Failure(err) != agentapi.Limit {
		t.Fatalf("check budget: %v", err)
	}
	if _, err := c.Thread(t.Context(), thread); err == nil {
		t.Fatal("exhausted epoch still usable")
	}
}
func TestUnreadableArchivedSubtreeInvalidatesCoverage(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, "sessions", thread, thread, nil, "")
	dir := filepath.Join(home, "archived_sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }()
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("user can read permission-denied directories")
	}
	c := New([]string{home}, Limits{})
	if s := catalogSet(t, c, thread); s.Complete {
		t.Fatal("permission failure claimed complete")
	}
}
func TestRevisionBindsHeaderIdentityOnRenewal(t *testing.T) {
	home := t.TempDir()
	fixture(t, home, "sessions", thread, thread, nil, "")
	first := catalogSet(t, New([]string{home}, Limits{}), thread)
	fixture(t, home, "sessions", thread, thread, map[string]any{"cwd": "/new/project"}, "")
	second := catalogSet(t, New([]string{home}, Limits{}), thread)
	if first.Revision == second.Revision {
		t.Fatal("identity change reused selection token")
	}
}

func TestApprovedHomeReplacementDoesNotWidenExistingOpener(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "approved")
	original := fixture(t, home, "sessions", thread, thread, nil, "")
	c := New([]string{home}, Limits{})
	files := c.Files()
	if err := os.Rename(home, home+"-original"); err != nil {
		t.Fatal(err)
	}
	fixture(t, home, "sessions", thread, thread, nil, "")
	if f, err := files.OpenRegular(original); err == nil {
		_ = f.Close()
		t.Fatal("replacement root gained old authority")
	}
}

func TestPrefixProofRejectsChangesSinceHeaderCapture(t *testing.T) {
	t.Parallel()
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace_%t", replace), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := fixture(t, home, "sessions", thread, thread, nil, "{\"ordinal\":1}\n")
			c := New([]string{home}, Limits{})
			c.readHeader(t.Context(), home, path)
			if len(c.files) != 1 {
				t.Fatal("header not captured")
			}
			if replace {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
			}
			extra := map[string]any{"cwd": "/synthetic/changed-project"}
			if replace {
				extra = nil
			}
			fixture(t, home, "sessions", thread, thread, extra, "{\"ordinal\":1}\n")
			if _, ok := c.prefix(t.Context(), c.files[0], c.files[0].info.Size()); ok {
				t.Fatal("prefix proof accepted a different header observation")
			}
		})
	}
}

func TestCheckRejectsRetargetedApprovedHome(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home, other, alias := filepath.Join(parent, "home"), filepath.Join(parent, "other"), filepath.Join(parent, "alias")
	fixture(t, home, "sessions", thread, thread, nil, "")
	fixture(t, other, "sessions", thread, thread, nil, "")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	c := New([]string{alias}, Limits{})
	s := catalogSet(t, c, thread)
	if !s.Complete {
		t.Fatal("initial inventory incomplete")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if err := c.Check(t.Context(), thread, s.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("retargeted source authority accepted: %v", err)
	}
}

func TestAuthorityDescriptorRejectsSymlinkStoreComponents(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := fixture(t, home, "sessions", thread, thread, nil, "")
	c := New([]string{home}, Limits{})
	c.initializeAuthority()
	approved := c.authorities[0]
	if err := os.Rename(filepath.Dir(path), filepath.Join(home, "private")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(home, "sessions")); err != nil {
		t.Fatal(err)
	}
	// Model a component changed after opener canonicalization and before the
	// descriptor open. The inner capability must reject it before any data read.
	if f, err := openAuthorityRegular(approved, path); err == nil {
		_ = f.Close()
		t.Fatal("descriptor open followed a replaced store component")
	}
}

func TestNativeVFSRejectsRetargetedApprovedHome(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home, alias := filepath.Join(parent, "home"), filepath.Join(parent, "alias")
	fixture(t, home, "sessions", thread, thread, nil, "")
	if err := os.WriteFile(filepath.Join(home, "state_5.sqlite"), []byte("synthetic database bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	c := New([]string{alias}, Limits{})
	c.initializeAuthority()
	approved := c.authorities[0]
	fsys := indexFS{home: approved.home, root: approved.root, info: approved.info, ctx: t.Context()}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if f, err := fsys.Open("state_5.sqlite"); err == nil {
		_ = f.Close()
		t.Fatal("native VFS retained retargeted source authority")
	}
}
