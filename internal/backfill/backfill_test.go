package backfill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// fixedNow is the planning clock in every test.
var fixedNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))

// fixedCreated is every file's birth time.
var fixedCreated = time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)

// tree is a constructed file system: root holds home and a temporary
// directory, so nothing depends on where t.TempDir lives.
type tree struct {
	t          *testing.T
	root, home string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := &tree{t: t, root: root, home: filepath.Join(root, "home")}
	tr.mkdir("home")
	tr.mkdir("tmp")
	return tr
}

func (tr *tree) path(rel string) string { return filepath.Join(tr.root, rel) }

func (tr *tree) mkdir(rel string) string {
	tr.t.Helper()
	p := tr.path(rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	return p
}

func (tr *tree) write(rel, content string) string {
	tr.t.Helper()
	p := tr.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
	return p
}

// repo makes rel a git repository.
func (tr *tree) repo(rel string) string {
	tr.mkdir(filepath.Join(rel, ".git"))
	return tr.path(rel)
}

// worktree makes rel a linked worktree of the repository at repoRel, laid
// out as git does it.
func (tr *tree) worktree(repoRel, rel, name string) string {
	gitDir := tr.mkdir(filepath.Join(repoRel, ".git", "worktrees", name))
	tr.write(filepath.Join(repoRel, ".git", "worktrees", name, "commondir"), "../..\n")
	rp, err := filepath.Rel(tr.path(rel), gitDir)
	if err != nil {
		tr.t.Fatal(err)
	}
	tr.write(filepath.Join(rel, ".git"), "gitdir: "+rp+"\n")
	return tr.path(rel)
}

func (tr *tree) env() Environment {
	return Environment{
		Home:     tr.home,
		TempDirs: []string{tr.path("tmp")},
		Now:      func() time.Time { return fixedNow },
		// Birth times are whatever the test run's clock says; pin them.
		FileCreated: func(string) (time.Time, error) { return fixedCreated, nil },
		Workers:     2,
	}
}

// Synthetic transcripts, shaped like internal/archive/testdata's fixtures.

func claudeTranscript(sessionID, cwd string, start time.Time) string {
	return fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"please check it"}}
{"type":"assistant","uuid":"b","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Checked."}]}}
`, sessionID, cwd, start.UTC().Format(time.RFC3339), sessionID, cwd, start.Add(time.Minute).UTC().Format(time.RFC3339))
}

func codexTranscript(id, sessionID, cwd string, start time.Time) string {
	return fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"session_id":%q,"timestamp":%q,"cwd":%q,"source":"cli","originator":"codex_cli_rs"}}
{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the file"}]}}
`, start.UTC().Format(time.RFC3339), id, sessionID, start.UTC().Format(time.RFC3339), cwd, start.Add(time.Minute).UTC().Format(time.RFC3339))
}

const cursorTranscript = `{"role":"user","message":{"content":[{"type":"text","text":"Summarize the failing widget test."}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"It times out."}]}}
`

func claudeFile(slug, id string) string {
	return filepath.Join(".claude", "projects", slug, id+".jsonl")
}

func codexFile(id string) string {
	return filepath.Join(".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+id+".jsonl")
}

// states is a fake ArchiveState.
type states map[string]SkipReason

func (s states) Classify(_, nativeSessionID string) (SkipReason, error) {
	return s[nativeSessionID], nil
}

func plan(t *testing.T, env Environment, st ArchiveState, cfg config.Config, filters Filters) Plan {
	t.Helper()
	if st == nil {
		st = states{}
	}
	p, err := BuildPlan(context.Background(), env, st, cfg, filters)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func candidate(t *testing.T, p Plan, nativeID string) Candidate {
	t.Helper()
	for _, c := range p.Candidates {
		if c.NativeSessionID == nativeID {
			return c
		}
	}
	t.Fatalf("no candidate %q among %d", nativeID, len(p.Candidates))
	return Candidate{}
}

func project(root string, included bool) archive.ProjectActivation {
	return archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: included}
}

func TestResolveRules(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	tr.mkdir("home/repo/sub")
	wt := tr.worktree("home/repo", "home/repo/.claude/worktrees/live", "live")
	tr.mkdir("home/repo/.claude/worktrees/live/pkg")
	tr.repo("home/sub-parent")
	sub := tr.path("home/sub-parent/module")
	tr.mkdir("home/sub-parent/.git/modules/module")
	tr.write("home/sub-parent/module/.git", "gitdir: ../.git/modules/module\n")
	tr.mkdir("home/notes")
	tr.mkdir("home/.git") // a dotfiles repository in home claims nothing
	scratch := tr.mkdir("home/Library/Application Support/Claude/scratch-workspaces/a-b/scratch-1")
	scratchRoot := filepath.Dir(filepath.Dir(scratch))
	proj := tr.mkdir("home/proj")
	inner := tr.mkdir("home/proj/inner")
	tmp := tr.mkdir("tmp/run-1")

	included := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(proj, true), project(inner, false)}}}
	homeExcluded := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tr.home, false)}}}

	cases := []struct {
		name    string
		cfg     config.Config
		filters Filters
		cwd     string
		want    resolution
	}{
		{"repository", config.Config{}, Filters{}, filepath.Join(repo, "sub"), resolution{root: repo, kind: ProjectKindRepository}},
		{"worktree folds into its repository", config.Config{}, Filters{}, filepath.Join(wt, "pkg"), resolution{root: repo, kind: ProjectKindRepository}},
		{"missing Claude worktree maps by path", config.Config{}, Filters{}, filepath.Join(repo, ".claude", "worktrees", "gone", "x"), resolution{root: repo, kind: ProjectKindRepository}},
		{"missing Codex worktree", config.Config{}, Filters{}, filepath.Join(tr.home, ".codex", "worktrees", "ab12", "repo"), resolution{skip: SkipWorktreeUnresolved}},
		{"missing Cursor worktree", config.Config{}, Filters{}, filepath.Join(tr.home, ".cursor", "worktrees", "repo", "x1"), resolution{skip: SkipWorktreeUnresolved}},
		{"submodule is its own repository", config.Config{}, Filters{}, sub, resolution{root: sub, kind: ProjectKindRepository}},
		{"nearest configured ancestor", included, Filters{}, filepath.Join(proj, "other"), resolution{root: proj, kind: ProjectKindDirectory, included: true}},
		{"nearer excluded project wins", included, Filters{}, filepath.Join(inner, "x"), resolution{root: inner, kind: ProjectKindDirectory, skip: SkipExcludedProject}},
		{"exclusion beats the home default", homeExcluded, Filters{IncludeHome: true}, tr.home, resolution{root: tr.home, kind: ProjectKindHome, skip: SkipExcludedProject}},
		{"scratch chats share one project", config.Config{}, Filters{}, scratch, resolution{root: scratchRoot, kind: ProjectKindScratch}},
		{"temporary directory", config.Config{}, Filters{}, tmp, resolution{root: tmp, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}},
		{"temporary directory with --include-temp", config.Config{}, Filters{IncludeTemp: true}, tmp, resolution{root: tmp, kind: ProjectKindTemporary}},
		{"home", config.Config{}, Filters{}, tr.home, resolution{root: tr.home, kind: ProjectKindHome, skip: SkipHomeDirectory}},
		{"home with --include-home", config.Config{}, Filters{IncludeHome: true}, tr.home, resolution{root: tr.home, kind: ProjectKindHome}},
		{"above home", config.Config{}, Filters{IncludeHome: true}, tr.root, resolution{root: tr.root, kind: ProjectKindHome, skip: SkipHomeDirectory}},
		{"root", config.Config{}, Filters{}, "/", resolution{root: "/", kind: ProjectKindHome, skip: SkipHomeDirectory}},
		{"plain folder under a dotfiles home", config.Config{}, Filters{}, filepath.Join(tr.home, "notes"), resolution{root: filepath.Join(tr.home, "notes"), kind: ProjectKindDirectory}},
		{"missing folder", config.Config{}, Filters{}, filepath.Join(tr.home, "gone", "deeper"), resolution{root: filepath.Join(tr.home, "gone", "deeper"), kind: ProjectKindDirectory}},
		{"no working directory", config.Config{}, Filters{}, "", resolution{skip: SkipProjectUnknown}},
		{"relative working directory", config.Config{}, Filters{}, "repo", resolution{skip: SkipProjectUnknown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := newResolver(tr.env(), tc.cfg, tc.filters).resolve(tc.cwd)
			if got != tc.want {
				t.Fatalf("resolve(%q) = %+v, want %+v", tc.cwd, got, tc.want)
			}
		})
	}
}

func TestCursorSlugs(t *testing.T) {
	tr := newTree(t)
	tr.repo("home/a-b/c")
	tr.repo("home/a/b-c")
	one := tr.repo("home/site_v2.io")
	gone := filepath.Join(tr.home, "gone-app")
	tr.write("home/Library/Application Support/Cursor/User/workspaceStorage/0f3a/workspace.json", fmt.Sprintf(`{"folder":%q}`, "file://"+gone))
	ambiguous := cursorSlug(filepath.Join(tr.home, "a-b", "c"))
	if ambiguous != cursorSlug(filepath.Join(tr.home, "a", "b-c")) {
		t.Fatal("test setup: the two folders must share a slug")
	}
	chats := map[string]string{"chat-two": ambiguous, "chat-one": cursorSlug(one), "chat-none": cursorSlug(filepath.Join(tr.home, "nowhere")), "chat-ws": cursorSlug(gone)}
	for id, slug := range chats {
		tr.write(filepath.Join("home", ".cursor", "projects", slug, "agent-transcripts", id, id+".jsonl"), cursorTranscript)
	}
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})

	if c := candidate(t, p, "chat-one"); c.Skip != "" || c.ProjectRoot != one || c.StartedAtSource != StartedAtSourceFileCreated {
		t.Fatalf("one match: %+v", c)
	}
	if c := candidate(t, p, "chat-two"); c.Skip != SkipProjectUnknown {
		t.Fatalf("two matches: %+v", c)
	}
	if c := candidate(t, p, "chat-none"); c.Skip != SkipProjectUnknown {
		t.Fatalf("no match: %+v", c)
	}
	if c := candidate(t, p, "chat-ws"); c.Skip != "" || c.ProjectRoot != gone || c.ProjectExists {
		t.Fatalf("workspace.json match: %+v", c)
	}
}

// A Cursor slug also matches a root that only a Claude Code session names.
func TestCursorSlugFromClaudeRoot(t *testing.T) {
	tr := newTree(t)
	gone := filepath.Join(tr.home, "old-repo")
	tr.write(filepath.Join("home", claudeFile("home-old-repo", "c1")), claudeTranscript("c1", gone, fixedNow.Add(-48*time.Hour)))
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(gone), "agent-transcripts", "k1", "k1.jsonl"), cursorTranscript)
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, "k1"); c.Skip != "" || c.ProjectRoot != gone {
		t.Fatalf("got %+v", c)
	}
}

func TestIdentityAndClassification(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-72 * time.Hour)
	const good = "0a9b3c4d-0000-4000-8000-000000000001"
	const other = "0a9b3c4d-0000-4000-8000-000000000002"
	files := map[string]string{
		claudeFile("s", "ok"):                             claudeTranscript("ok", repo, start),
		claudeFile("s", "renamed"):                        claudeTranscript("someone-else", repo, start),
		claudeFile("s", "forked"):                         claudeTranscript("forked", repo, start) + claudeTranscript("parent", repo, start.Add(-time.Hour)),
		claudeFile("s", "zero"):                           "",
		claudeFile("s", "summary"):                        `{"type":"summary","summary":"recap","leafUuid":"a","cwd":"` + repo + `"}` + "\n",
		claudeFile("s", "unknown"):                        `{"type":"file-history-snapshot","cwd":"` + repo + `","snapshot":{}}` + "\n",
		claudeFile("s", "future"):                         claudeTranscript("future", repo, fixedNow.Add(time.Hour)),
		codexFile(good):                                   codexTranscript(good, good, repo, start),
		codexFile(other):                                  codexTranscript(good, good, repo, start),
		codexFile("0a9b3c4d-0000-4000-8000-000000000003"): codexTranscript("0a9b3c4d-0000-4000-8000-000000000003", "0a9b3c4d-0000-4000-8000-000000000004", repo, start),
	}
	for rel, content := range files {
		tr.write(filepath.Join("home", rel), content)
	}
	big := tr.write(filepath.Join("home", claudeFile("s", "big")), claudeTranscript("big", repo, start))
	if err := os.Truncate(big, archive.MaxRecordBytes+1); err != nil {
		t.Fatal(err)
	}
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	want := map[string]SkipReason{
		// An empty file records no working directory, and project_unknown
		// comes first in the spec's order.
		"ok": "", "renamed": SkipIdentityMismatch, "forked": "", "zero": SkipProjectUnknown,
		"summary": SkipEmpty, "unknown": SkipUnsafeFormat, "future": SkipStartInFuture, "big": SkipTooLarge,
	}
	for id, reason := range want {
		if c := candidate(t, p, id); c.Skip != reason {
			t.Errorf("%s: skip %q, want %q", id, c.Skip, reason)
		}
	}
	codex := map[string]SkipReason{}
	for _, c := range p.Candidates {
		if c.Harness == "codex" {
			codex[filepath.Base(c.TranscriptPath)] = c.Skip
		}
	}
	if len(codex) != 3 || codex[filepath.Base(codexFile(good))] != "" || codex[filepath.Base(codexFile(other))] != SkipIdentityMismatch || codex[filepath.Base(codexFile("0a9b3c4d-0000-4000-8000-000000000003"))] != SkipIdentityMismatch {
		t.Fatalf("codex skips: %v", codex)
	}
	if c := candidate(t, p, "ok"); !c.StartedAt.Equal(start.UTC().Truncate(time.Second)) || c.StartedAtSource != StartedAtSourceTranscript || c.ProjectRoot != repo {
		t.Fatalf("ok: %+v", c)
	}
	if c := candidate(t, p, "forked"); !c.StartedAt.Equal(start.Add(-time.Hour).UTC().Truncate(time.Second)) {
		t.Fatalf("start is the earliest record: %+v", c)
	}
}

// Codex: sessions/ wins over archived_sessions/, and the session_meta
// timestamp is the start.
func TestCodexDiscovery(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "0a9b3c4d-0000-4000-8000-00000000000a"
	start := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	body := codexTranscript(id, "", repo, start)
	tr.write(filepath.Join("home", codexFile(id)), body)
	tr.write(filepath.Join("home", ".codex", "archived_sessions", filepath.Base(codexFile(id))), body)
	const archivedOnly = "0a9b3c4d-0000-4000-8000-00000000000b"
	tr.write(filepath.Join("home", ".codex", "archived_sessions", filepath.Base(codexFile(archivedOnly))), codexTranscript(archivedOnly, archivedOnly, repo, start))
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if len(p.Candidates) != 2 {
		t.Fatalf("got %d candidates", len(p.Candidates))
	}
	c := candidate(t, p, id)
	if c.Skip != "" || !strings.Contains(c.TranscriptPath, filepath.Join(".codex", "sessions")) || !c.StartedAt.Equal(start) {
		t.Fatalf("got %+v", c)
	}
	if c := candidate(t, p, archivedOnly); c.Skip != "" {
		t.Fatalf("archived only: %+v", c)
	}
}

func TestClaudeSubagents(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "parent")), claudeTranscript("parent", repo, start))
	tr.write(filepath.Join("home", ".claude", "projects", "s", "parent", "subagents", "agent-a1.jsonl"), claudeTranscript("parent", repo, start))
	tr.write(filepath.Join("home", ".claude", "projects", "s", "parent", "subagents", "agent-a1.meta.json"), `{}`)
	tr.write(filepath.Join("home", ".claude", "projects", "s", "parent", "subagents", "agent-a2.jsonl"), claudeTranscript("parent", repo, start))
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if len(p.Candidates) != 1 {
		t.Fatalf("subagents must not be sessions of their own: %d candidates", len(p.Candidates))
	}
	if subs := candidate(t, p, "parent").Subagents; len(subs) != 2 {
		t.Fatalf("subagents: %+v", subs)
	}
}

func TestFiltersAndPrecedence(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	other := tr.repo("home/other")
	excluded := tr.repo("home/excluded")
	tmp := tr.mkdir("tmp/x")
	day := func(d int) time.Time { return time.Date(2026, 9, d, 18, 0, 0, 0, time.UTC) }
	sessions := map[string]struct {
		cwd   string
		start time.Time
	}{
		"in-range":          {repo, day(18)},
		"before-range":      {repo, day(10)},
		"other-project":     {other, day(18)},
		"archived-and-old":  {repo, day(10)},
		"excluded-and-old":  {excluded, day(10)},
		"excluded-in-range": {excluded, day(18)},
		"home":              {tr.home, day(18)},
		"temp":              {tmp, day(18)},
		"removed":           {repo, day(18)},
		"unadmitted":        {repo, day(18)},
	}
	for id, s := range sessions {
		tr.write(filepath.Join("home", claudeFile("s", id)), claudeTranscript(id, s.cwd, s.start))
	}
	tr.write(filepath.Join("home", codexFile("0a9b3c4d-0000-4000-8000-0000000000cc")), codexTranscript("0a9b3c4d-0000-4000-8000-0000000000cc", "", repo, day(18)))
	// A temporary session whose IDs also disagree: the earlier reason wins.
	tr.write(filepath.Join("home", claudeFile("s", "temp-mismatch")), claudeTranscript("nope", tmp, day(18)))
	st := states{"archived-and-old": SkipAlreadyArchived, "removed": SkipRemovedByUndo, "unadmitted": SkipRegisteredNotAdmitted}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(excluded, false)}}}

	t.Run("no filters", func(t *testing.T) {
		p := plan(t, tr.env(), st, cfg, Filters{})
		want := map[string]SkipReason{
			"in-range": "", "before-range": "", "other-project": "", "archived-and-old": SkipAlreadyArchived,
			"excluded-and-old": SkipExcludedProject, "excluded-in-range": SkipExcludedProject,
			"home": SkipHomeDirectory, "temp": SkipTemporaryDirectory, "removed": SkipRemovedByUndo,
			"unadmitted": SkipRegisteredNotAdmitted, "temp-mismatch": SkipTemporaryDirectory,
		}
		for id, reason := range want {
			if c := candidate(t, p, id); c.Skip != reason {
				t.Errorf("%s: %q, want %q", id, c.Skip, reason)
			}
		}
		if p.Skipped()[SkipFilteredOut] != 0 {
			t.Error("filtered_out without filters")
		}
	})
	t.Run("filters", func(t *testing.T) {
		p := plan(t, tr.env(), st, cfg, Filters{Harnesses: []string{"claude-code"}, Projects: []string{repo, excluded, tr.home}, Since: "2026-09-15", Until: "2026-09-20", IncludeRemoved: true})
		want := map[string]SkipReason{
			"in-range": "", "before-range": SkipFilteredOut, "other-project": SkipFilteredOut,
			"archived-and-old": SkipAlreadyArchived, "excluded-and-old": SkipFilteredOut,
			"excluded-in-range": SkipExcludedProject, "home": SkipHomeDirectory, "temp": SkipFilteredOut,
			"removed": "", "unadmitted": SkipRegisteredNotAdmitted,
		}
		for id, reason := range want {
			if c := candidate(t, p, id); c.Skip != reason {
				t.Errorf("%s: %q, want %q", id, c.Skip, reason)
			}
		}
		if c := candidate(t, p, "0a9b3c4d-0000-4000-8000-0000000000cc"); c.Skip != SkipFilteredOut {
			t.Errorf("codex with --harness claude: %q", c.Skip)
		}
	})
	t.Run("until is inclusive in local time", func(t *testing.T) {
		// day(18) is 11:00 PDT on the 18th.
		p := plan(t, tr.env(), st, cfg, Filters{Since: "2026-09-18", Until: "2026-09-18"})
		if c := candidate(t, p, "in-range"); c.Skip != "" {
			t.Fatalf("got %q", c.Skip)
		}
	})
}

func TestFilterValidation(t *testing.T) {
	for _, f := range []Filters{
		{Harnesses: []string{"vim"}}, {Since: "09/01/2026"}, {Until: "2026-13-01"},
		{Since: "2026-09-10", Until: "2026-09-01"}, {Projects: []string{""}},
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v: no error", f)
		}
	}
}

func TestCursorDatabaseCount(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/site")
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(repo), "agent-transcripts", "k1", "k1.jsonl"), cursorTranscript)
	env := tr.env()
	var got map[string]bool
	env.CursorDatabaseOnly = func(_ context.Context, chats map[string]bool) (int, bool, error) {
		got = chats
		return 2, true, nil
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if !got["k1"] || p.Skipped()[SkipCursorDatabaseOnly] != 2 || p.Found() != 3 {
		t.Fatalf("chats %v, skipped %v, found %d", got, p.Skipped(), p.Found())
	}
	if p := plan(t, env, nil, config.Config{}, Filters{Harnesses: []string{"claude"}}); p.Skipped()[SkipFilteredOut] != 3 {
		t.Fatalf("filtered: %v", p.Skipped())
	}
}

func TestByteBudget(t *testing.T) {
	b := newByteBudget(100)
	if n := b.acquire(500); n != 100 {
		t.Fatalf("an oversized file takes the whole budget, got %d", n)
	}
	done := make(chan struct{})
	go func() {
		b.release(b.acquire(1))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second file ran beside an oversized one")
	case <-time.After(20 * time.Millisecond):
	}
	b.release(100)
	<-done
}
