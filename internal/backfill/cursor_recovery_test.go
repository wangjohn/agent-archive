package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

func removeFirstRunFileWitness(t *testing.T, tr *tree) {
	t.Helper()
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseSoleDestinationAndHiddenClone(t *testing.T) {
	for _, clone := range []bool{false, true} {
		t.Run(map[bool]string{false: "sole", true: "hidden_clone"}[clone], func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)}}
			composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}
			if clone {
				other := tr.repo("home/clone")
				chats = append(chats, CursorDatabaseChat{ID: "clone", Folder: other, CreatedAt: fixedNow.Add(-48 * time.Hour)})
				composers["clone"] = syntheticChat("clone", nil, "hi")
			}
			calls := 0
			env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, &calls)
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, Projects: []string{root}})
			c := candidate(t, p, goneID)
			if clone {
				if c.ProjectResolution != nil || c.ProjectRoot != "" {
					t.Fatal(c)
				}
			} else {
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatal(c)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
					t.Fatal(err)
				}
				if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != root {
					t.Fatal(cfg.Archive.Projects)
				}
			}
			if len(databaseCandidates(p)) != 0 {
				t.Fatal("hidden DB output leaked")
			}
			if calls > 3 {
				t.Fatalf("per-thread catalog observations: %d", calls)
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseUnavailableOrMalformed(t *testing.T) {
	for _, kind := range []string{"locked", "unknown", "error", "malformed", "unknown_root", "rows"} {
		t.Run(kind, func(t *testing.T) {
			_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}
			if kind == "malformed" {
				chats[0].Malformed = true
			}
			if kind == "unknown_root" {
				chats[0].Folder = ""
			}
			if kind == "rows" {
				chats = make([]CursorDatabaseChat, cursorRecoveryRows+1)
			}
			env.CursorDatabase = fakeCursorDatabase(chats, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, nil)
			if kind == "locked" || kind == "unknown" {
				env.CursorDatabase = func(context.Context) (CursorDatabaseResult, error) {
					return CursorDatabaseResult{Reason: CursorUncheckedLocked}, nil
				}
			}
			if kind == "error" {
				env.CursorDatabase = func(context.Context) (CursorDatabaseResult, error) {
					return CursorDatabaseResult{}, errors.New("unavailable")
				}
			}
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			c := candidate(t, p, goneID)
			if c.ProjectResolution != nil || c.Skip == "" {
				t.Fatal(c)
			}
			if kind == "rows" {
				epoch, err := readCursorRecoveryEpoch(t.Context(), env, newResolver(env, cfg, Filters{}), unreadable{})
				if err != nil || !epoch.budget || epoch.complete {
					t.Fatal(epoch, err)
				}
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseEpochRenewsMembershipRootAndEligibility(t *testing.T) {
	for _, kind := range []string{"membership", "root", "eligibility"} {
		t.Run(kind, func(t *testing.T) {
			tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			chats := []CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}
			composers := map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}
			env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
				return fakeCursorDatabase(chats, composers, nil, nil)(ctx)
			}
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "membership":
				chats = append(chats, CursorDatabaseChat{ID: "clone", Folder: tr.repo("home/clone"), CreatedAt: fixedNow})
				composers["clone"] = syntheticChat("clone", nil, "hi")
			case "root":
				chats[0].Folder = tr.repo("home/other")
			case "eligibility":
				composers["live"] = syntheticChat("live", nil)
			}
			if err := p.CheckRecovery(t.Context()); err == nil {
				t.Fatal("changed DB evidence admitted")
			}
		})
	}
}

func TestFirstRunRecoveryCursorDatabaseSettledObservationAvoidsChatRereads(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	path := CursorStateDatabase(tr.home)
	writeCursorDB(t, path, true, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + root}}, "hi"))
	reader := CursorRecoveryDatabaseReaderFor(env)
	catalogs, reads := 0, 0
	env.CursorRecoveryDatabase = func(ctx context.Context, rows int, bytes int64) (CursorDatabaseResult, error) {
		catalogs++
		res, err := reader(ctx, rows, bytes)
		read := res.ReadRecoverySnapshot
		if read != nil {
			res.ReadRecoverySnapshot = func(ctx context.Context, id string, budget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
				reads++
				return read(ctx, id, budget)
			}
		}
		return res, err
	}
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	for range 4 {
		if err := p.CheckRecovery(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if catalogs != 1 || reads != 1 {
		t.Fatalf("settled DB reread: catalogs=%d chats=%d", catalogs, reads)
	}
	db := openCursorWriter(t, path, true)
	insertCursorRows(t, db, chatRows("other", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + tr.repo("home/clone")}}, "hi"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("changed settled DB admitted")
	}
	if catalogs != 1 || reads != 1 {
		t.Fatal("changed settled DB must require a new reviewed plan")
	}
}

func TestFirstRunRecoveryCursorDatabaseBoundedRenewalsAndAggregateBytes(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	calls := 0
	env.CursorDatabase = fakeCursorDatabase([]CursorDatabaseChat{{ID: "live", Folder: root, CreatedAt: fixedNow}}, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, &calls)
	env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	for range cursorRecoveryRenewals {
		if err := p.CheckRecovery(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("unbounded renewed DB scans")
	}
	if calls != cursorRecoveryRenewals+1 {
		t.Fatal(calls)
	}
	b := cursorRecoveryReadBudget{remaining: 12}
	c := cursorstore.Composer{Composer: []byte("1234"), Bubbles: []cursorstore.Bubble{{Value: []byte("5678")}}}
	if err := b.charge(c); err != nil || b.remaining != 4 {
		t.Fatal(b, err)
	}
	if err := b.charge(c); err == nil || !b.exhausted {
		t.Fatal("aggregate byte cap ignored")
	}
}

// The synthetic catalog is already in memory; this test port enforces membership
// and value allowances before returning borrowed composer bytes.
func boundedTestRecoveryReader(reader func(context.Context) (CursorDatabaseResult, error)) func(context.Context, int, int64) (CursorDatabaseResult, error) {
	return func(ctx context.Context, rows int, bytes int64) (CursorDatabaseResult, error) {
		res, err := reader(ctx)
		if err != nil || !res.Checked {
			return res, err
		}
		if len(res.Chats) > rows {
			return CursorDatabaseResult{RecoveryBudgetExhausted: true}, nil
		}
		res.ReadRecoverySnapshot = func(ctx context.Context, id string, budget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
			c, err := res.ReadChat(ctx, id)
			if err != nil {
				return c, nil, err
			}
			b := budget
			size := int64(len(c.Composer))
			for _, bubble := range c.Bubbles {
				size += int64(len(bubble.Value))
			}
			if b.Charge(1, size) != nil {
				return cursorstore.Composer{}, nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
			}
			return c, nil, nil
		}
		return res, nil
	}
}

func TestFirstRunRecoveryCursorDatabaseCatalogBoundBeforeAllocation(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/repo")
	path := CursorStateDatabase(tr.home)
	writeCursorDB(t, path, true, mergeRows(chatRows("a", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + root}}, "hi"), chatRows("b", nil, "hi")))
	env := tr.env()
	read := CursorRecoveryDatabaseReaderFor(env)
	for _, tc := range []struct {
		name  string
		rows  int
		bytes int64
	}{{"oversize_row", 4, 10}, {"many_rows", 1, 128 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := read(t.Context(), tc.rows, tc.bytes)
			if err != nil || res.Checked || !res.RecoveryBudgetExhausted || res.RecoveryRows > 4 || res.RecoveryBytes > tc.bytes {
				t.Fatal(res, err)
			}
		})
	}
	res, err := read(t.Context(), 4, 128<<20)
	if err != nil || !res.Checked || res.RecoveryRows != 4 || res.RecoveryBytes <= 0 {
		t.Fatal(res, err)
	}
	if _, snap, err := res.ReadRecoverySnapshot(t.Context(), "a", &cursorRecoveryReadBudget{remaining: 1, rows: cursorRecoveryRecords}); err == nil || snap != nil || !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatal("snapshot exceeded allowance", err)
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseMissingBoundedCapabilityDoesNotFallback(t *testing.T) {
	_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
	calls := 0
	env.CursorDatabase = fakeCursorDatabase([]CursorDatabaseChat{{ID: "live", Folder: root}}, map[string]cursorstore.Composer{"live": syntheticChat("live", nil, "hi")}, nil, &calls)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if c := candidate(t, p, goneID); c.ProjectResolution != nil || c.Skip == "" {
		t.Fatal(c)
	}
	if calls != 0 {
		t.Fatal("unbounded evidence fallback", calls)
	}
	cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
	p = plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, ProjectMappings: map[string]string{filepath.Join(env.Home, ".codex", "worktrees", "deleted", "repo"): root}})
	if c := candidate(t, p, goneID); c.Skip != "" || c.ProjectResolution == nil || c.ProjectResolution.Method != "explicit_mapping" {
		t.Fatal(c)
	}
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRunRecoveryCursorDatabaseSettledOwnershipCannotChange(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	removeFirstRunFileWitness(t, tr)
	folder := tr.mkdir("home/repo/inner")
	path := CursorStateDatabase(tr.home)
	writeCursorDB(t, path, false, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"uri": "file://" + folder}}, "hi"))
	env.CursorRecoveryDatabase = CursorRecoveryDatabaseReaderFor(env)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	if len(p.Imported()) != 1 || p.Imported()[0].ProjectRoot != root {
		t.Fatal(p.Candidates)
	}
	tr.mkdir("home/repo/inner/.git")
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("changed live ownership retained old destination")
	}
}

func TestFirstRunRecoveryCatalogLengthAndValuesShareTransaction(t *testing.T) {
	tr := newTree(t)
	path := CursorStateDatabase(tr.home)
	db := openCursorWriter(t, path, true)
	defer closeAtEnd(t, db)
	insertCursorRows(t, db, map[string]any{"composerData:a": "a", "composerData:b": "b"})
	writer := openCursorWriter(t, path, true)
	defer closeAtEnd(t, writer)
	h := &recoveryCatalogHost{db: db, maxRows: 2, budget: &cursorRecoveryReadBudget{remaining: 4096, rows: 100}}
	seen := 0
	const query = `SELECT key,value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'`
	if err := h.Query(t.Context(), query, func(agentapi.DatabaseRecord) error {
		seen++
		if seen == 1 {
			_, err := writer.ExecContext(t.Context(), `INSERT INTO cursorDiskKV(key,value)VALUES('composerData:c','c')`)
			return err
		}
		return nil
	}); err != nil || seen != 2 {
		t.Fatal("catalog snapshot changed during value allocation", seen, err)
	}
	h = &recoveryCatalogHost{db: db, maxRows: 2, budget: &cursorRecoveryReadBudget{remaining: 4096, rows: 100}}
	seen = 0
	if err := h.Query(t.Context(), query, func(agentapi.DatabaseRecord) error { seen++; return nil }); !agentapi.HasFailure(err, agentapi.Limit) || seen != 0 {
		t.Fatal("oversized membership allocated values", seen, err)
	}
}

func TestFirstRunRecoveryWorkspaceEvidenceIsBoundedAndRooted(t *testing.T) {
	for _, kind := range []string{"valid", "oversize", "symlink", "unbounded_port"} {
		t.Run(kind, func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			removeFirstRunFileWitness(t, tr)
			rel := "home/Library/Application Support/Cursor/User/workspaceStorage/current/workspace.json"
			path := tr.write(rel, fmt.Sprintf(`{"folder":%q}`, "file://"+root))
			switch kind {
			case "oversize":
				tr.write(rel, strings.Repeat("x", (1<<20)+1))
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := tr.write("home/outside.json", fmt.Sprintf(`{"folder":%q}`, "file://"+root))
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "unbounded_port":
				env.ReadFile = os.ReadFile
			}
			writeCursorDB(t, CursorStateDatabase(tr.home), false, chatRows("live", map[string]any{"workspaceIdentifier": map[string]any{"id": "current"}}, "hi"))
			env.CursorRecoveryDatabase = CursorRecoveryDatabaseReaderFor(env)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			c := candidate(t, p, goneID)
			if kind == "valid" {
				if c.Skip != "" || c.ProjectRoot != root {
					t.Fatal(c)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if c.ProjectResolution != nil || c.Skip == "" {
				t.Fatal("unsafe metadata invented destination", c)
			}
		})
	}
}
