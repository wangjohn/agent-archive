package backfill

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

var updateGolden = flag.Bool("update", false, "rewrite internal/backfill/testdata golden files")

// A worktree outside its repository goes through rule 2 on the repository it
// maps to: excluded with it, or imported under its configured spelling.
func TestWorktreeOutsideRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	feature := tr.worktree("home/repo", "home/repo-feature", "repo-feature")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	aliased := filepath.Join(tr.path("alias"), "repo")
	cases := []struct {
		name string
		cfg  config.Config
		want resolution
	}{
		{"unconfigured", config.Config{}, resolution{root: repo, kind: ProjectKindRepository}},
		{"excluded", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, false)}}}, resolution{root: repo, kind: ProjectKindRepository, skip: SkipExcludedProject}},
		{"included", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, true)}}}, resolution{root: repo, kind: ProjectKindRepository, included: true}},
		{"included through a symlinked spelling", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(aliased, true)}}}, resolution{root: aliased, kind: ProjectKindRepository, included: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), tc.cfg, Filters{}).resolve(feature); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A worktree whose git directory is gone is not a repository of its own.
func TestWorktreeWithMissingRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	codexWT := tr.path("home/.codex/worktrees/ab12/repo")
	tr.write("home/.codex/worktrees/ab12/repo/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "x")+"\n")
	claudeWT := tr.path("home/repo/.claude/worktrees/w1")
	tr.write("home/repo/.claude/worktrees/w1/.git", "gitdir: ../../../.git/worktrees/w1\n")
	cases := []struct {
		name, cwd string
		want      resolution
	}{
		{"Codex worktree", codexWT, resolution{skip: SkipWorktreeUnresolved}},
		{"Claude worktree maps by path", claudeWT, resolution{root: repo, kind: ProjectKindRepository}},
		{"missing worktree directly under home", filepath.Join(tr.home, ".claude", "worktrees", "x"), resolution{root: tr.home, kind: ProjectKindHome, skip: SkipHomeDirectory}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(tc.cwd); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A folder that no longer exists under a symlinked parent resolves through
// the parent, so its spelling and configured owner match the real path.
func TestMissingFolderUnderSymlink(t *testing.T) {
	tr := newTree(t)
	proj := tr.mkdir("home/proj")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	r := newResolver(tr.env(), config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(proj, true)}}}, Filters{})
	if got, want := r.resolve(filepath.Join(tr.path("alias"), "gone", "x")), (resolution{root: filepath.Join(tr.home, "gone", "x"), kind: ProjectKindDirectory}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := r.resolve(filepath.Join(tr.path("alias"), "proj", "missing")); got.root != proj || !got.included {
		t.Fatalf("configured owner through a symlink: %+v", got)
	}
}

func TestDuplicateSessions(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	// Two files named s1: the one whose records carry s1 is kept.
	tr.write(filepath.Join("home", claudeFile("a", "s1")), claudeTranscript("s1", repo, start)+claudeTranscript("s1", repo, start))
	tr.write(filepath.Join("home", claudeFile("b", "s1")), claudeTranscript("other", repo, start))
	// Two good files named s2: the larger is kept.
	tr.write(filepath.Join("home", claudeFile("a", "s2")), claudeTranscript("s2", repo, start))
	big := tr.write(filepath.Join("home", claudeFile("b", "s2")), claudeTranscript("s2", repo, start)+claudeTranscript("s2", repo, start))
	// A Codex session in sessions/ and, under another name, archived_sessions/.
	const id = "0a9b3c4d-0000-4000-8000-0000000000dd"
	active := tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, start))
	tr.write(filepath.Join("home", ".codex", "archived_sessions", "rollout-2026-09-19T10-00-00-"+id+".jsonl"), codexTranscript(id, id, repo, start)+codexTranscript(id, id, repo, start))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	kept := map[string]string{}
	for _, c := range p.Candidates {
		switch c.Skip {
		case "":
			if kept[c.NativeSessionID] != "" {
				t.Fatalf("%s imported twice", c.NativeSessionID)
			}
			kept[c.NativeSessionID] = c.TranscriptPath
		case SkipDuplicateSession:
		default:
			t.Errorf("%s: unexpected skip %q", c.TranscriptPath, c.Skip)
		}
	}
	if want := tr.path(filepath.Join("home", claudeFile("a", "s1"))); kept["s1"] != want {
		t.Errorf("s1 kept %s", kept["s1"])
	}
	if kept["s2"] != big {
		t.Errorf("s2 kept %s", kept["s2"])
	}
	if kept[id] != active {
		t.Errorf("codex kept %s", kept[id])
	}
	if got := p.Skipped()[SkipDuplicateSession]; got != 3 {
		t.Errorf("duplicates: %d", got)
	}
}

// A file that disappears during planning is not counted at all; one that
// grows past the limit after discovery is too_large.
func TestFilesChangingMidScan(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	before := tr.write(filepath.Join("home", claudeFile("s", "gone-before")), claudeTranscript("gone-before", repo, start))
	during := tr.write(filepath.Join("home", claudeFile("s", "gone-during")), claudeTranscript("gone-during", repo, start))
	grows := tr.write(filepath.Join("home", claudeFile("s", "grows")), claudeTranscript("grows", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "stays")), claudeTranscript("stays", repo, start))
	env := tr.env()
	env.Open = func(path string) (io.ReadCloser, error) {
		switch path {
		case before:
			return nil, os.ErrNotExist
		case during:
			f, err := os.Open(path)
			if err == nil {
				err = os.Remove(path)
			}
			return f, err
		case grows:
			if err := os.Truncate(path, archive.MaxRecordBytes+1); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if len(p.Candidates) != 2 {
		t.Fatalf("got %d candidates, want grows and stays", len(p.Candidates))
	}
	if c := candidate(t, p, "grows"); c.Skip != SkipTooLarge {
		t.Fatalf("grows: %q", c.Skip)
	}
	if c := candidate(t, p, "stays"); c.Skip != "" {
		t.Fatalf("stays: %q", c.Skip)
	}
}

// Discovery never follows a symlink out of an app's store.
func TestSymlinkedTranscriptsSkipped(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	elsewhere := tr.write("elsewhere/x.jsonl", claudeTranscript("linked", repo, fixedNow.Add(-time.Hour)))
	for _, link := range []string{
		filepath.Join("home", claudeFile("s", "linked")),
		filepath.Join("home", ".cursor", "projects", cursorSlug(repo), "agent-transcripts", "k1", "k1.jsonl"),
	} {
		tr.mkdir(filepath.Dir(link))
		if err := os.Symlink(elsewhere, tr.path(link)); err != nil {
			t.Fatal(err)
		}
	}
	if p := plan(t, tr.env(), nil, config.Config{}, Filters{}); len(p.Candidates) != 0 {
		t.Fatalf("symlinks were followed: %+v", p.Candidates)
	}
}

// Cursor's text form is imported through the collector's text filter; when a
// chat has both forms, the JSONL file wins.
func TestCursorTextForm(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/site")
	dir := filepath.Join("home", ".cursor", "projects", cursorSlug(repo), "agent-transcripts")
	tr.write(filepath.Join(dir, "t1.txt"), "user: summarize the widget test\nassistant: it times out\n")
	tr.write(filepath.Join(dir, "t2.txt"), "user: hello\n")
	json := tr.write(filepath.Join(dir, "t2", "t2.jsonl"), cursorTranscript)
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if len(p.Candidates) != 2 {
		t.Fatalf("got %d candidates", len(p.Candidates))
	}
	if c := candidate(t, p, "t1"); c.Skip != "" || c.ProjectRoot != repo || c.StartedAtSource != archive.StartedAtSourceFileCreated {
		t.Fatalf("text form: %+v", c)
	}
	if c := candidate(t, p, "t2"); c.TranscriptPath != json {
		t.Fatalf("both forms: %+v", c)
	}
}

// Header reads pass over lines too long or malformed to be a header.
func TestReadHeadBounds(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	huge := strings.Repeat("x", headLineLimit+10)
	tr.write(filepath.Join("home", claudeFile("s", "long-first")), `{"type":"user","pad":"`+huge+`"}`+"\n"+claudeTranscript("long-first", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "malformed-first")), "{not json\n"+claudeTranscript("malformed-first", repo, start))
	const id = "0a9b3c4d-0000-4000-8000-0000000000ee"
	tr.write(filepath.Join("home", codexFile(id)), "{not json\n"+codexTranscript(id, id, repo, start))
	const hugeID = "0a9b3c4d-0000-4000-8000-0000000000ef"
	tr.write(filepath.Join("home", codexFile(hugeID)), strings.Replace(codexTranscript(hugeID, hugeID, repo, start), `"source":"cli"`, `"source":"`+huge+`"`, 1))
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	for _, id := range []string{"long-first", "malformed-first", id} {
		if c := candidate(t, p, id); c.Skip != "" || c.ProjectRoot != repo {
			t.Errorf("%s: %+v", id, c)
		}
	}
	for _, c := range p.Candidates {
		// Without its header the session has neither an ID nor a working
		// directory; project_unknown comes first.
		if strings.Contains(c.TranscriptPath, hugeID) && (c.Skip != SkipProjectUnknown || c.NativeSessionID != "") {
			t.Errorf("an oversized session_meta is no header: %+v", c)
		}
	}
}

func TestSubagentChecks(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "parent")), claudeTranscript("parent", repo, start))
	dir := filepath.Join("home", ".claude", "projects", "s", "parent", "subagents")
	tr.write(filepath.Join(dir, "agent-ok1.jsonl"), claudeTranscript("parent", repo, start))
	tr.write(filepath.Join(dir, "agent-bad.jsonl"), `{"type":"file-history-snapshot","snapshot":{}}`+"\n")
	big := tr.write(filepath.Join(dir, "agent-big.jsonl"), claudeTranscript("parent", repo, start))
	if err := os.Truncate(big, archive.MaxRecordBytes+1); err != nil {
		t.Fatal(err)
	}
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	c := candidate(t, p, "parent")
	if len(c.Subagents) != 1 || c.Subagents[0].AgentID != "ok1" || c.SubagentsSkipped != 2 || p.SubagentsSkipped() != 2 {
		t.Fatalf("subagents %+v, skipped %d", c.Subagents, c.SubagentsSkipped)
	}
}

// The plan is the same whatever the worker count.
func TestPlanDeterministic(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	for i, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		tr.write(filepath.Join("home", claudeFile("s", id)), claudeTranscript(id, repo, fixedNow.Add(-time.Duration(i+1)*time.Hour)))
		tr.write(filepath.Join("home", claudeFile("t", id)), claudeTranscript(id, filepath.Join(repo, "x"), fixedNow.Add(-time.Duration(i+1)*time.Hour)))
	}
	one, eight := tr.env(), tr.env()
	one.Workers, eight.Workers = 1, 8
	a, b := plan(t, one, nil, config.Config{}, Filters{}), plan(t, eight, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(a.Candidates, b.Candidates) {
		t.Fatal("plans differ between 1 and 8 workers")
	}
}

func TestPlanCancelled(t *testing.T) {
	tr := newTree(t)
	tr.write(filepath.Join("home", claudeFile("s", "a")), claudeTranscript("a", tr.home, fixedNow))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildPlan(ctx, tr.env(), states{}, config.Config{}, Filters{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// Every skip reason renders with its wording and override, in the spec's
// order. Regenerate with `go test ./internal/backfill -run
// TestSkipReasonsGolden -update` and review the diff.
func TestSkipReasonsGolden(t *testing.T) {
	start := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	p := Plan{
		GeneratedAt: fixedNow, Home: "/Users/p", RetentionDays: 90, Harnesses: []string{"claude"},
		Destination: Destination{Provider: "s3", Bucket: "bucket", Prefix: "agent-archive"},
		Candidates: []Candidate{
			{Harness: "claude", ProjectRoot: "/Users/p/repo", ProjectKind: ProjectKindRepository, ProjectExists: true, StartedAt: start, Bytes: 2000,
				Subagents: []Subagent{{AgentID: "a1", Bytes: 1000}}, SubagentsSkipped: 1},
			{Harness: "claude", ProjectRoot: "/Users/p", ProjectKind: ProjectKindHome, ProjectIncluded: true, ProjectExists: true, StartedAt: start, Bytes: 500},
		},
		CursorDatabaseOnly: 1, CursorDatabaseChecked: true,
	}
	for _, reason := range skipOrder {
		harness := "claude"
		if reason == SkipCursorDatabaseOnly {
			continue // counted by CursorDatabaseOnly
		}
		if reason == SkipProjectUnknown {
			harness = "cursor"
		}
		p.Candidates = append(p.Candidates, Candidate{Harness: harness, Skip: reason})
	}
	var out bytes.Buffer
	RenderText(&out, p)
	out.WriteString("\n--- cursor database not checked, --include-home set ---\n")
	p.CursorDatabaseOnly, p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, p.Filters.IncludeHome = 0, false, CursorUncheckedLocked, true
	p.Candidates = p.Candidates[:2]
	RenderText(&out, p)
	golden := filepath.Join("testdata", "skip-reasons.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("differs from %s:\n%s", golden, out.Bytes())
	}
	for _, reason := range skipOrder {
		if skipLabels[reason] == "" {
			t.Errorf("%s has no label", reason)
		}
	}
}
