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
	"github.com/wangjohn/agent-archive/internal/config"
)

// Discovery never follows a symlink out of an app's store.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestSymlinkedTranscriptsSkipped(t *testing.T) {
	t.Parallel()
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
//
// Regression: backfill review, 2026-09 (cbce179).
func TestCursorTextForm(t *testing.T) {
	t.Parallel()
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
//
// Regression: backfill review, 2026-09 (cbce179).
func TestReadHeadBounds(t *testing.T) {
	t.Parallel()
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

// The header scan reads at most headScanLimit bytes: a working directory
// first recorded beyond it is not found, and the session has no project.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestHeaderScanCap(t *testing.T) {
	t.Parallel()
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

// A folder in an app's store that cannot be listed is passed over and
// counted; the rest of the plan goes on, and the folder's path is never
// shown.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestUnreadableFolderIsCounted(t *testing.T) {
	t.Parallel()
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
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
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
