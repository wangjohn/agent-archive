package backfill

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
)

// A larger copy of a session that cannot be imported never displaces a copy
// that can: the good copy is imported and the bad one is the duplicate.
func TestDuplicateKeepsTheImportableCopy(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-24 * time.Hour)
	good := map[string]string{}
	for _, id := range []string{"too-large", "empty", "refused"} {
		good[id] = tr.write(filepath.Join("home", claudeFile("a", id)), claudeTranscript(id, repo, start))
	}
	big := tr.write(filepath.Join("home", claudeFile("b", "too-large")), claudeTranscript("too-large", repo, start))
	if err := os.Truncate(big, collector.DefaultMaxRawTranscriptBytes+1); err != nil {
		t.Fatal(err)
	}
	summary := `{"type":"summary","summary":"recap","leafUuid":"x","cwd":"` + repo + `"}` + "\n"
	tr.write(filepath.Join("home", claudeFile("b", "empty")), strings.Repeat(summary, 20))
	// No record the adapter recognizes: it refuses the file.
	tr.write(filepath.Join("home", claudeFile("b", "refused")), strings.Repeat(`{"type":"mystery","sessionId":"refused","cwd":"`+repo+`"}`+"\n", 20))

	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	for _, c := range p.Candidates {
		want := SkipDuplicateSession
		if c.TranscriptPath == good[c.NativeSessionID] {
			want = ""
		}
		if c.Skip != want {
			t.Errorf("%s (%s): skip %q, want %q", c.NativeSessionID, c.TranscriptPath, c.Skip, want)
		}
	}
	if got := len(p.Imported()); got != 3 {
		t.Fatalf("imported %d sessions, want 3", got)
	}
}

// A missing worktree under home maps to home by its path; when home itself
// is a configured, included project, that configuration wins.
func TestWorktreeMappedToConfiguredHome(t *testing.T) {
	tr := newTree(t)
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tr.home, true)}}}
	got := newResolver(tr.env(), cfg, Filters{}).resolve(filepath.Join(tr.home, ".claude", "worktrees", "gone"))
	if want := (resolution{root: tr.home, kind: ProjectKindHome, included: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An existing worktree outside the apps' worktree folders whose git
// directory is gone cannot be mapped to its repository.
func TestExistingWorktreeWithMissingGitDir(t *testing.T) {
	tr := newTree(t)
	wt := tr.mkdir("home/elsewhere/feature")
	tr.write("home/elsewhere/feature/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "feature")+"\n")
	if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(filepath.Join(wt, "pkg")); got != (resolution{skip: SkipWorktreeUnresolved}) {
		t.Fatalf("got %+v", got)
	}
}

// A blank native ID is an identity problem for that session, not a reason
// to abandon the whole plan.
func TestBlankNativeID(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "0a9b3c4d-0000-4000-8000-0000000000ab"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(" ", " ", repo, fixedNow.Add(-time.Hour)))
	p, err := BuildPlan(t.Context(), tr.env(), refusingBlank{}, config.Config{}, Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Candidates) != 1 || p.Candidates[0].Skip != SkipIdentityMismatch {
		t.Fatalf("candidates %+v", p.Candidates)
	}
}

// refusingBlank fails as the local store does for a blank native ID.
type refusingBlank struct{}

func (refusingBlank) Classify(_, nativeSessionID string) (SkipReason, error) {
	if strings.TrimSpace(nativeSessionID) == "" {
		return "", os.ErrInvalid
	}
	return "", nil
}

// A session retention removed is reported as removed_by_retention, ahead of
// the reasons after it, and --include-removed imports it.
func TestRemovedByRetentionInPlan(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("s", "removed")), claudeTranscript("removed", repo, start))
	tr.write(filepath.Join("home", claudeFile("s", "removed-home")), claudeTranscript("removed-home", tr.home, start))
	st := states{"removed": SkipRemovedByRetention, "removed-home": SkipRemovedByRetention}
	p := plan(t, tr.env(), st, config.Config{}, Filters{})
	for _, id := range []string{"removed", "removed-home"} {
		if c := candidate(t, p, id); c.Skip != SkipRemovedByRetention {
			t.Errorf("%s: %q", id, c.Skip)
		}
	}
	p = plan(t, tr.env(), st, config.Config{}, Filters{IncludeRemoved: true})
	if c := candidate(t, p, "removed"); c.Skip != "" || c.ProjectRoot != repo {
		t.Errorf("with --include-removed: %+v", c)
	}
	if c := candidate(t, p, "removed-home"); c.Skip != SkipHomeDirectory {
		t.Errorf("with --include-removed, the next reason applies: %q", c.Skip)
	}
}

// A Claude Code or Codex session whose records carry no timestamp is
// start_unknown, even though the file's creation time is available: only
// Cursor's start comes from the file. A date filter does not claim it.
func TestNoRecordTimeIsStartUnknown(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	tr.write(filepath.Join("home", claudeFile("s", "no-time")), `{"type":"user","uuid":"a","sessionId":"no-time","cwd":"`+repo+`","message":{"role":"user","content":"please check it"}}`+"\n")
	const codexID = "0a9b3c4d-0000-4000-8000-0000000000c1"
	tr.write(filepath.Join("home", codexFile(codexID)), `{"type":"session_meta","payload":{"id":"`+codexID+`","cwd":"`+repo+`","source":"cli"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the file"}]}}
`)
	env := tr.env()
	env.FileCreated = func(string) (time.Time, error) { return fixedNow.Add(-time.Hour), nil }
	for _, filters := range []Filters{{}, {Since: "2026-09-01"}} {
		p := plan(t, env, nil, config.Config{}, filters)
		for _, id := range []string{"no-time", codexID} {
			if c := candidate(t, p, id); c.Skip != SkipStartUnknown || !c.StartedAt.IsZero() || c.StartedAtSource != "" {
				t.Fatalf("%+v: %+v", filters, c)
			}
		}
	}
}

// A Claude Code conversation whose records carry no sessionId at all cannot
// be matched with a hook's registration: identity_mismatch. A file with no
// conversation and no ID is still reported as empty.
func TestClaudeWithoutSessionID(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	tr.write(filepath.Join("home", claudeFile("s", "no-id")), `{"type":"user","uuid":"a","cwd":"`+repo+`","timestamp":"2026-09-22T10:00:00Z","message":{"role":"user","content":"please check it"}}`+"\n")
	tr.write(filepath.Join("home", claudeFile("s", "no-id-summary")), `{"type":"summary","summary":"recap","cwd":"`+repo+`"}`+"\n")
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, "no-id"); c.Skip != SkipIdentityMismatch {
		t.Errorf("conversation without an ID: %q", c.Skip)
	}
	if c := candidate(t, p, "no-id-summary"); c.Skip != SkipEmpty {
		t.Errorf("no conversation and no ID: %q", c.Skip)
	}
}

// A folder in an app's store that cannot be listed is passed over and
// counted; the rest of the plan goes on, and the folder's path is never
// shown.
func TestUnreadableFolderIsCounted(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	tr.write(filepath.Join("home", claudeFile("good", "readable")), claudeTranscript("readable", repo, start))
	tr.write(filepath.Join("home", claudeFile("locked", "hidden")), claudeTranscript("hidden", repo, start))
	locked := filepath.Join(tr.home, ".claude", "projects", "locked")
	codexDay := filepath.Join(tr.home, ".codex", "sessions", "2026")
	tr.mkdir(filepath.Join("home", ".codex", "sessions", "2026"))
	env := tr.env()
	env.ReadDir = func(dir string) ([]fs.DirEntry, error) {
		if dir == locked || dir == codexDay {
			return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
		}
		return os.ReadDir(dir)
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if len(p.Candidates) != 1 || p.Candidates[0].NativeSessionID != "readable" || p.Candidates[0].Skip != "" {
		t.Fatalf("candidates %+v", p.Candidates)
	}
	if p.UnreadableFolders != 2 {
		t.Fatalf("unreadable folders %d, want 2", p.UnreadableFolders)
	}
	var text, js bytes.Buffer
	RenderText(&text, p)
	if err := RenderJSON(&js, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "   2  folders in the app stores could not be read\n") || !strings.Contains(js.String(), `"unreadable_folders": 2`) {
		t.Fatalf("text:\n%s\njson:\n%s", text.String(), js.String())
	}
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, "locked") || strings.Contains(out, ".codex") {
			t.Fatalf("an unreadable folder is named:\n%s", out)
		}
	}
}

// An app's whole store that cannot be listed is named, not hidden in the
// folder count: none of that app's sessions were found. The other apps'
// sessions are still planned, and no path is shown. A store the harness
// filter leaves out is not reported.
func TestUnreadableStoreIsNamed(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	const codexID, archivedID = "0a9b3c4d-0000-4000-8000-0000000000d1", "0a9b3c4d-0000-4000-8000-0000000000d2"
	tr.write(filepath.Join("home", claudeFile("s", "claude-1")), claudeTranscript("claude-1", repo, start))
	tr.write(filepath.Join("home", codexFile(codexID)), codexTranscript(codexID, codexID, repo, start))
	tr.write(filepath.Join("home", ".codex", "archived_sessions", filepath.Base(codexFile(archivedID))), codexTranscript(archivedID, archivedID, repo, start))
	tr.write(filepath.Join("home", ".cursor", "projects", "slug", "agent-transcripts", "k-1", "k-1.jsonl"), cursorTranscript)
	home := tr.home
	for _, tc := range []struct {
		name    string
		root    string
		app     string
		line    string
		missing []string
	}{
		{"claude", filepath.Join(home, ".claude", "projects"), "claude", "Claude Code's session folder could not be read (check permissions);\n      none of its sessions are included.", []string{"claude-1"}},
		{"codex", filepath.Join(home, ".codex", "sessions"), "codex", "Codex's session folder could not be read (check permissions);\n      none of its sessions are included.", []string{codexID}},
		{"codex archived", filepath.Join(home, ".codex", "archived_sessions"), "codex", "Codex's archived session folder could not be read (check permissions);\n      none of its archived sessions are included.", []string{archivedID}},
		{"cursor", filepath.Join(home, ".cursor", "projects"), "cursor", "Cursor's session folder could not be read (check permissions);\n      none of its sessions are included.", []string{"k-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tr.env()
			env.ReadDir = func(dir string) ([]fs.DirEntry, error) {
				if dir == tc.root {
					return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
				}
				return os.ReadDir(dir)
			}
			p := plan(t, env, nil, config.Config{}, Filters{})
			if strings.Join(p.UnreadableStores, ",") != tc.app || p.UnreadableFolders != 0 {
				t.Fatalf("stores %v, folders %d", p.UnreadableStores, p.UnreadableFolders)
			}
			found := map[string]bool{}
			for _, c := range p.Candidates {
				found[c.NativeSessionID] = true
			}
			if len(p.Candidates) != 4-len(tc.missing) || found[tc.missing[0]] {
				t.Fatalf("candidates %+v", p.Candidates)
			}
			var text, js bytes.Buffer
			RenderText(&text, p)
			if err := RenderJSON(&js, p); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), "\n      "+tc.line+"\n") || !strings.Contains(js.String(), `"unreadable_stores": [`+"\n"+`    "`+tc.app+`"`) {
				t.Fatalf("text:\n%s\njson:\n%s", text.String(), js.String())
			}
			for _, out := range []string{text.String(), js.String()} {
				if strings.Contains(out, ".claude") || strings.Contains(out, ".codex") || strings.Contains(out, ".cursor") {
					t.Fatalf("a store path is shown:\n%s", out)
				}
			}
			other := "claude"
			if tc.app == "claude" {
				other = "codex"
			}
			if p := plan(t, env, nil, config.Config{}, Filters{Harnesses: []string{other}}); len(p.UnreadableStores) != 0 {
				t.Fatalf("a filtered-out store is reported: %v", p.UnreadableStores)
			}
		})
	}
}

// The header scan reads at most headScanLimit bytes: a working directory
// first recorded beyond it is not found, and the session has no project.
func TestHeaderScanCap(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	line := `{"type":"summary","summary":"` + strings.Repeat("x", headLineLimit/2) + `"}` + "\n"
	lead := strings.Repeat(line, headScanLimit/len(line)+1)
	tr.write(filepath.Join("home", claudeFile("s", "late-cwd")), lead+claudeTranscript("late-cwd", repo, fixedNow.Add(-time.Hour)))
	tr.write(filepath.Join("home", claudeFile("s", "early-cwd")), claudeTranscript("early-cwd", repo, fixedNow.Add(-time.Hour))+lead)
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, "late-cwd"); c.Skip != SkipProjectUnknown {
		t.Errorf("late cwd: %q", c.Skip)
	}
	if c := candidate(t, p, "early-cwd"); c.Skip != "" || c.ProjectRoot != repo {
		t.Errorf("early cwd: %+v", c)
	}
}
