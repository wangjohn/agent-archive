package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// A workspace reference that fails to read keeps the epoch incomplete even when
// a single message folder would otherwise name the chat's root.
func TestCursorRecoveryUnreadableWorkspaceStaysGapDespiteMessageFolder(t *testing.T) {
	_, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	env.ReadFile = os.ReadFile // recovery refuses the unbounded port
	works := []*work{{t: &transcript{harness: harnessCursor}, chat: CursorDatabaseChat{ID: "c", WorkspaceID: "current"}, messageFolders: []string{root}}}
	complete, gaps, err := observeCursorRecoveryWorks(t.Context(), env, newResolver(env, cfg, Filters{}), works, &cursorRecoveryReadBudget{remaining: 1 << 20, rows: 16})
	if err != nil || complete || !gaps[recoveryGap{Agent: string(harnessCursor), Cause: CauseCursorChatFolderUnavailable}] {
		t.Fatalf("complete=%v gaps=%+v err=%v", complete, gaps, err)
	}
	if works[0].res.root != root {
		t.Fatalf("message folder not used: %+v", works[0].res)
	}
}

// noGapCause is a recovery failure that no witness gap explains.
const noGapCause DiagnosticDetail = ""

// Chats with no folder evidence (subagent composers, a chat opened without a
// workspace) name no root, so they cannot hide a clone. A workspace reference
// whose workspace.json is missing still could, so it stays a gap.
func TestFirstRunRecoveryFolderlessCursorChatsAreNotWitnesses(t *testing.T) {
	for _, kind := range []string{"folderless", "unreadable_workspace"} {
		t.Run(kind, func(t *testing.T) {
			_, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			chats := []CursorDatabaseChat{
				{ID: "live", Folder: root, CreatedAt: fixedNow.Add(-48 * time.Hour)},
				{ID: "task-call-00000000-0000-0000-0000-000000000001-1", CreatedAt: fixedNow.Add(-48 * time.Hour)},
				{ID: "top-level", CreatedAt: fixedNow.Add(-48 * time.Hour)},
			}
			if kind == "unreadable_workspace" {
				chats[2].WorkspaceID = "missing"
			}
			composers := map[string]cursorstore.Composer{}
			for _, chat := range chats {
				composers[chat.ID] = syntheticChat(chat.ID, nil, "hi")
			}
			env.CursorDatabase = fakeCursorDatabase(chats, composers, nil, nil)
			env.CursorRecoveryDatabase = boundedTestRecoveryReader(env.CursorDatabase)
			p := plan(t, env, nil, cfg, Filters{})
			c := candidate(t, p, goneID)
			if kind == "folderless" {
				if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
					t.Fatalf("folderless chat blocked recovery: %+v %+v", c, c.Diagnostic)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal(err)
				}
				return
			}
			if c.ProjectResolution != nil || c.Skip != SkipWorktreeUnresolved || c.Diagnostic == nil || c.Diagnostic.Detail != CauseCursorChatFolderUnavailable {
				t.Fatalf("unreadable workspace certified uniqueness: %+v %+v", c, c.Diagnostic)
			}
		})
	}
}

func TestCursorChatFolderlessRequiresNoEvidence(t *testing.T) {
	for _, tc := range []struct {
		chat    CursorDatabaseChat
		folders []string
		want    bool
	}{
		{CursorDatabaseChat{}, nil, true},
		{CursorDatabaseChat{Folder: "/a"}, nil, false},
		{CursorDatabaseChat{WorkspaceID: "w"}, nil, false},
		{CursorDatabaseChat{}, []string{"/a"}, false},
		{CursorDatabaseChat{}, []string{"/a", "/b"}, false},
	} {
		if got := cursorChatFolderless(tc.chat, tc.folders); got != tc.want {
			t.Errorf("%+v %v: got %v", tc.chat, tc.folders, got)
		}
	}
}

func TestRecoveryWitnessGapsNameTheirCause(t *testing.T) {
	file := func(edit func(*work)) *work {
		w := &work{t: &transcript{harness: harnessCodex}, res: resolution{root: "/r", kind: ProjectKindRepository}}
		edit(w)
		return w
	}
	database := func(edit func(*work)) *work {
		w := &work{t: &transcript{harness: harnessCursor}, c: Candidate{SourceKind: archive.SourceKindCursorSQLite}, res: resolution{root: "/r", kind: ProjectKindRepository}}
		edit(w)
		return w
	}
	for name, tc := range map[string]struct {
		w    *work
		want DiagnosticDetail
		gap  bool
	}{
		"usable":                {file(func(*work) {}), "", false},
		"changed":               {file(func(w *work) { w.sourceChanged = true }), CauseNativeInventoryChanged, true},
		"vanished":              {file(func(w *work) { w.vanished = true }), CauseNativeInventoryChanged, true},
		"unsafe":                {file(func(w *work) { w.unsafe = true }), CauseSessionFolderUnknown, true},
		"mismatch":              {file(func(w *work) { w.t.identityMismatch = true }), CauseSessionFolderUnknown, true},
		"file unknown":          {file(func(w *work) { w.res = resolution{skip: SkipProjectUnknown} }), CauseSessionFolderUnknown, true},
		"database vanished":     {database(func(w *work) { w.vanished = true }), CauseCursorDatabaseUnavailable, true},
		"database unsafe":       {database(func(w *work) { w.unsafe = true }), CauseCursorDatabaseUnavailable, true},
		"database folderless":   {database(func(w *work) { w.res = resolution{skip: SkipProjectUnknown} }), "", false},
		"database workspace":    {database(func(w *work) { w.res, w.chat.WorkspaceID = resolution{skip: SkipProjectUnknown}, "w" }), CauseCursorChatFolderUnavailable, true},
		"database folders":      {database(func(w *work) { w.res, w.messageFolders = resolution{skip: SkipProjectUnknown}, []string{"/a", "/b"} }), CauseCursorChatFolderUnavailable, true},
		"database named folder": {database(func(w *work) { w.res, w.chat.Folder = resolution{skip: SkipProjectUnknown}, "relative" }), CauseCursorChatFolderUnavailable, true},
	} {
		if cause, gap := witnessGap(tc.w); cause != tc.want || gap != tc.gap {
			t.Errorf("%s: got %q %v", name, cause, gap)
		}
	}

	var gaps recoveryGaps
	if gaps.cause() != "" {
		t.Fatal("empty gaps named a cause")
	}
	gaps.addUnreadable(unreadable{folders: 2, folderAgents: map[string]bool{"claude": true}})
	gaps.add(string(harnessCursor), CauseCursorChatFolderUnavailable)
	if !gaps[recoveryGap{Agent: "claude", Cause: CauseNativeStoreUnreadable}] || gaps.cause() != CauseNativeStoreUnreadable {
		t.Fatalf("%+v", gaps)
	}
	gaps.add("", CauseRecoveryBudget)
	if gaps.cause() != CauseRecoveryBudget {
		t.Fatal("budget must select the budget outcome")
	}
	var stores recoveryGaps
	stores.addUnreadable(unreadable{stores: map[string]bool{"codex": true, "claude": false}})
	if len(stores) != 1 || !stores[recoveryGap{Agent: "codex", Cause: CauseNativeStoreUnreadable}] {
		t.Fatalf("%+v", stores)
	}
	var unattributed recoveryGaps
	unattributed.addUnreadable(unreadable{folders: 1})
	if unattributed.cause() != CauseNativeStoreUnreadable {
		t.Fatalf("%+v", unattributed)
	}
}

// The resolver reports which witness evidence was missing instead of the
// generic configured-repository text, and keeps the budget outcome distinct.
func TestRecoveryInventoryGapCauseReachesResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unread  unreadable
		db      recoveryGaps
		changed bool
		outcome sourcefacts.RecoveryOutcome
		cause   DiagnosticDetail
	}{
		{"store", unreadable{stores: map[string]bool{"claude": true}}, nil, false, sourcefacts.RecoveryInventoryUnavailable, CauseNativeStoreUnreadable},
		{"inventory", unreadable{}, nil, true, sourcefacts.RecoveryInventoryUnavailable, CauseNativeInventoryChanged},
		{"database", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseCursorDatabaseUnavailable}: true}, false, sourcefacts.RecoveryInventoryUnavailable, CauseCursorDatabaseUnavailable},
		{"budget", unreadable{}, recoveryGaps{{Agent: "cursor", Cause: CauseRecoveryBudget}: true}, false, sourcefacts.RecoveryBudgetExhausted, ""},
		{"complete", unreadable{}, nil, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
			gone := tr.path("home/.codex/worktrees/deleted/repo")
			key := archive.RepoKey("https://example.test/acme/repo")
			cfg.Archive.Projects = []archive.ProjectActivation{project(root, true)}
			r := newResolver(env, cfg, Filters{})
			r.inventoryCurrent = func(context.Context) bool { return !tc.changed }
			prepareRecoveryInventory(t.Context(), r, nil, tc.unread, tc.db)
			res := r.resolveEvidence(t.Context(), gone, key)
			if res.outcome != tc.outcome || res.cause != tc.cause {
				t.Fatalf("got %q/%q, want %q/%q", res.outcome, res.cause, tc.outcome, tc.cause)
			}
			if d := candidateDiagnostic(res.skip, res.outcome, res.cause); tc.cause != "" && (d == nil || d.Detail != tc.cause) {
				t.Fatalf("diagnostic %+v", d)
			}
		})
	}
}

func TestRecoveryGapDiagnosticsRenderInTextAndJSON(t *testing.T) {
	for _, cause := range recoveryGapCauses {
		t.Run(string(cause), func(t *testing.T) {
			d := candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryInventoryUnavailable, cause)
			if d == nil || d.Detail != cause {
				t.Fatalf("%+v", d)
			}
			p := Plan{Candidates: []Candidate{{Harness: "codex", NativeSessionID: "n", Skip: SkipWorktreeUnresolved, Diagnostic: d}}}
			var text, structured bytes.Buffer
			RenderText(&text, p)
			if err := RenderJSON(&structured, p); err != nil {
				t.Fatal(err)
			}
			if want := diagnosticDescriptions[cause].text; want == "" || !strings.Contains(text.String(), want) {
				t.Fatalf("text missing %q:\n%s", want, text.String())
			}
			var got struct {
				Diagnostics []DiagnosticSummary `json:"diagnostics"`
			}
			if err := json.Unmarshal(structured.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Diagnostics) != 1 || got.Diagnostics[0].Detail != cause || got.Diagnostics[0].Count != 1 {
				t.Fatalf("%+v", got)
			}
		})
	}
	// A cause never replaces a decided outcome other than inventory.
	if d := candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryAmbiguous, CauseNativeStoreUnreadable); string(d.Detail) != string(sourcefacts.RecoveryAmbiguous) {
		t.Fatal(d)
	}
}
