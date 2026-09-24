package backfill

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Codex desktop's dated workspaces, ~/Documents/Codex/<date>/<name>, are one
// project, like Claude desktop's scratch chats, unless a repository inside
// one claims the session first.
func TestCodexDesktopWorkspaces(t *testing.T) {
	tr := newTree(t)
	codexDocs := tr.mkdir("home/Documents/Codex")
	first := tr.mkdir("home/Documents/Codex/2026-09-20/plan-trip")
	second := tr.mkdir("home/Documents/Codex/2026-09-21/fix-bug")
	cloned := tr.repo("home/Documents/Codex/2026-09-22/cloned")
	start := fixedNow.Add(-48 * time.Hour)
	ids := []string{
		"0a9b3c4d-0000-4000-8000-0000000000c1",
		"0a9b3c4d-0000-4000-8000-0000000000c2",
		"0a9b3c4d-0000-4000-8000-0000000000c3",
	}
	for i, cwd := range []string{first, second, filepath.Join(cloned, "src")} {
		tr.write(filepath.Join("home", ".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-0"+string(rune('0'+i))+"-"+ids[i]+".jsonl"), codexTranscript(ids[i], ids[i], cwd, start))
	}
	p := plan(t, tr.env(), nil, config.Config{Harnesses: []string{"codex"}}, Filters{})
	for _, id := range ids[:2] {
		if c := candidate(t, p, id); c.Skip != "" || c.ProjectRoot != codexDocs || c.ProjectKind != ProjectKindScratch {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if c := candidate(t, p, ids[2]); c.ProjectRoot != cloned || c.ProjectKind != ProjectKindRepository {
		t.Errorf("repository inside a workspace: %+v", c)
	}
	// A configured ~/Documents/Codex is still a workspace folder.
	r := newResolver(tr.env(), config.Config{}, Filters{})
	if r.kindOf(codexDocs) != ProjectKindScratch {
		t.Errorf("kind of the folder itself: %s", r.kindOf(codexDocs))
	}

	var out bytes.Buffer
	RenderText(&out, p)
	text := out.String()
	notRepo := strings.Index(text, "Not a repository:")
	row := strings.Index(text, "Codex desktop workspaces")
	if notRepo < 0 || row < notRepo {
		t.Fatalf("the workspaces are not listed under Not a repository:\n%s", text)
	}
	if !strings.Contains(text, "Chats in the workspaces Codex creates for them. New ones will be captured too.") {
		t.Fatalf("no note:\n%s", text)
	}
	if strings.Contains(text, "2026-09-20") && strings.Contains(text, "plan-trip") {
		t.Fatal("a workspace's own folder is shown")
	}
}
