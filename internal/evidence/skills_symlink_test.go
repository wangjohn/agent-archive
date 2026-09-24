package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// skillLayout is a temporary home, project, and an unrelated directory
// outside both, standing in for ~/.aws or another repository.
type skillLayout struct {
	home    string
	project string
	outside string
}

func newSkillLayout(t *testing.T) skillLayout {
	t.Helper()
	dir := t.TempDir()
	l := skillLayout{home: filepath.Join(dir, "home"), project: filepath.Join(dir, "repo"), outside: filepath.Join(dir, "outside")}
	for _, d := range []string{l.home, l.project, l.outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// observeClaudeSkills returns the inventory and snapshots for one scope.
func observeClaudeSkills(t *testing.T, l skillLayout, scope string) (map[string]any, []map[string]any, string) {
	t.Helper()
	got, err := ObserveSkills(SkillOptions{Harness: "claude", ProjectRoot: l.project, UserHome: l.home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	var inventory map[string]any
	var snapshots []map[string]any
	for _, e := range got {
		if e.Payload["scope"] != scope {
			continue
		}
		switch e.Kind {
		case archive.EvidenceKindSkillInventory:
			inventory = e.Payload
		case archive.EvidenceKindSkillSnapshot:
			snapshots = append(snapshots, e.Payload)
		case archive.EvidenceKindSkillDiscovered, archive.EvidenceKindSkillInvocation, archive.EvidenceKindSkillRead,
			archive.EvidenceKindLifecycleHook, archive.EvidenceKindFinalResponse, archive.EvidenceKindExplicitFeedback,
			archive.EvidenceKindLinkedSession, archive.EvidenceKindCaptureGap:
			// Only the skill inventory and snapshots are under test.
		}
	}
	return inventory, snapshots, string(encoded)
}

// A cloned repository must not be able to archive a file outside itself by
// shipping a SKILL.md, or a skill directory, that is a symlink to it. Both
// count as uninspected; nothing of the target is read.
func TestObserveSkillsIgnoresProjectSkillLinkedOutsideProject(t *testing.T) {
	l := newSkillLayout(t)
	credentials := filepath.Join(l.outside, ".aws", "credentials")
	writeSkillFile(t, credentials, "[default]\naws_secret_access_key = wJalrSYNTHETIC\nsentinel-outside-file\n")
	writeSkillFile(t, filepath.Join(l.outside, "other-repo", "skills", "x", "SKILL.md"), "---\nname: other\n---\nsentinel-other-repo\n")
	symlink(t, credentials, filepath.Join(l.project, ".claude", "skills", "file-link", "SKILL.md"))
	symlink(t, filepath.Join(l.outside, "other-repo", "skills", "x"), filepath.Join(l.project, ".claude", "skills", "dir-link"))
	writeSkillFile(t, filepath.Join(l.project, ".claude", "skills", "real", "SKILL.md"), "---\nname: real\n---\nbody\n")

	inventory, snapshots, all := observeClaudeSkills(t, l, "project_claude")
	for _, leaked := range []string{"sentinel-outside-file", "sentinel-other-repo", "wJalrSYNTHETIC"} {
		if strings.Contains(all, leaked) {
			t.Fatalf("%q archived from outside the project: %s", leaked, all)
		}
	}
	if len(snapshots) != 1 || snapshots[0]["name"] != "real" {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if inventory["inventory_complete"] != false || inventory["omitted_count"] != float64(2) {
		t.Fatalf("inventory = %#v", inventory)
	}
}

// A project skill must not archive a file of the project's own that is not
// a skill, such as its .env.
func TestObserveSkillsIgnoresProjectSkillLinkedToNonSkillFileInProject(t *testing.T) {
	l := newSkillLayout(t)
	writeSkillFile(t, filepath.Join(l.project, ".env"), "DB_PASSWORD=hunter2\nsentinel-dotenv\n")
	symlink(t, filepath.Join("..", "..", "..", ".env"), filepath.Join(l.project, ".claude", "skills", "env", "SKILL.md"))

	inventory, snapshots, all := observeClaudeSkills(t, l, "project_claude")
	if strings.Contains(all, "sentinel-dotenv") || len(snapshots) != 0 || inventory["omitted_count"] != float64(1) {
		t.Fatalf("inventory=%#v snapshots=%#v", inventory, snapshots)
	}
}

// A repository controls its skill root too: with .claude/skills itself a
// link to the project (or a folder in it), "inside the skill root" would
// admit any project file. A project skill must be a file named SKILL.md.
func TestObserveSkillsIgnoresProjectFileBehindALinkedSkillRoot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rootTarget string
		secret     string
		link       string
	}{
		{"root is the project", "..", ".env", filepath.Join("envskill", "SKILL.md")},
		{"root is a project folder", filepath.Join("..", "config"), filepath.Join("config", "master.key"), filepath.Join("k", "SKILL.md")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newSkillLayout(t)
			writeSkillFile(t, filepath.Join(l.project, tc.secret), "0123456789abcdef-sentinel-project-secret\n")
			symlink(t, tc.rootTarget, filepath.Join(l.project, ".claude", "skills"))
			root, err := filepath.EvalSymlinks(filepath.Join(l.project, ".claude", "skills"))
			if err != nil {
				t.Fatal(err)
			}
			target, err := filepath.Rel(filepath.Join(root, filepath.Dir(tc.link)), filepath.Join(l.project, tc.secret))
			if err != nil {
				t.Fatal(err)
			}
			symlink(t, target, filepath.Join(root, tc.link))

			_, snapshots, all := observeClaudeSkills(t, l, "project_claude")
			if strings.Contains(all, "sentinel-project-secret") || len(snapshots) != 0 {
				t.Fatalf("project file archived through a linked skill root: %s", all)
			}
		})
	}
}

// Run from the home directory, the project's .claude/skills is the user's
// ~/.claude/skills. It is observed once, under the user scope and its rules,
// so the project rule (anything inside the project, here all of $HOME) never
// admits a link to ~/.aws/credentials.
func TestObserveSkillsProjectRootAtHomeUsesUserRules(t *testing.T) {
	l := newSkillLayout(t)
	credentials := filepath.Join(l.home, ".aws", "credentials")
	writeSkillFile(t, credentials, "[default]\naws_secret_access_key = wJalrSYNTHETIC\nsentinel-home-credentials\n")
	symlink(t, credentials, filepath.Join(l.home, ".claude", "skills", "creds", "SKILL.md"))
	writeSkillFile(t, filepath.Join(l.home, ".claude", "skills", "real", "SKILL.md"), "---\nname: real\n---\nbody\n")
	l.project = l.home

	got, err := ObserveSkills(SkillOptions{Harness: "claude", ProjectRoot: l.project, UserHome: l.home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "sentinel-home-credentials") || strings.Contains(string(encoded), "wJalrSYNTHETIC") {
		t.Fatalf("home credentials archived: %s", encoded)
	}
	snapshots := 0
	for _, e := range got {
		if e.Payload["scope"] == "project_claude" {
			t.Fatalf("the home skills directory was observed twice: %#v", e)
		}
		if e.Kind == archive.EvidenceKindSkillSnapshot {
			snapshots++
		}
	}
	if snapshots != 1 {
		t.Fatalf("snapshots = %d, want only the real skill", snapshots)
	}
}

// Links that stay inside the repository keep working, such as one skills
// directory shared by several harnesses' skill roots.
func TestObserveSkillsFollowsProjectSkillLinkWithinProject(t *testing.T) {
	l := newSkillLayout(t)
	writeSkillFile(t, filepath.Join(l.project, "skills", "shared", "SKILL.md"), "---\nname: shared\n---\nbody\n")
	symlink(t, filepath.Join("..", "..", "skills", "shared"), filepath.Join(l.project, ".claude", "skills", "shared"))

	inventory, snapshots, _ := observeClaudeSkills(t, l, "project_claude")
	if len(snapshots) != 1 || snapshots[0]["name"] != "shared" || inventory["inventory_complete"] != true {
		t.Fatalf("inventory=%#v snapshots=%#v", inventory, snapshots)
	}
}

// A user-level skill directory linked into a skills checkout elsewhere is a
// supported way to install a skill; a user-level SKILL.md linked to a file
// that is not a SKILL.md is not a skill and is not read.
func TestObserveSkillsUserSkillLinksMustResolveToASkill(t *testing.T) {
	l := newSkillLayout(t)
	writeSkillFile(t, filepath.Join(l.outside, "skills-repo", "linked", "SKILL.md"), "---\nname: linked\n---\nbody\n")
	symlink(t, filepath.Join(l.outside, "skills-repo", "linked"), filepath.Join(l.home, ".claude", "skills", "linked"))
	notes := filepath.Join(l.outside, "notes.txt")
	writeSkillFile(t, notes, "sentinel-notes\n")
	symlink(t, notes, filepath.Join(l.home, ".claude", "skills", "notes", "SKILL.md"))

	inventory, snapshots, all := observeClaudeSkills(t, l, "user_claude")
	if strings.Contains(all, "sentinel-notes") {
		t.Fatalf("non-skill file archived: %s", all)
	}
	if len(snapshots) != 1 || snapshots[0]["name"] != "linked" || inventory["omitted_count"] != float64(1) {
		t.Fatalf("inventory=%#v snapshots=%#v", inventory, snapshots)
	}
}

// A SKILL.md that resolves to something other than a regular file (here a
// directory named SKILL.md) is uninspected rather than opened.
func TestObserveSkillsNonRegularSkillFileIsUninspected(t *testing.T) {
	l := newSkillLayout(t)
	if err := os.MkdirAll(filepath.Join(l.project, ".claude", "skills", "odd", "SKILL.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	inventory, snapshots, _ := observeClaudeSkills(t, l, "project_claude")
	if len(snapshots) != 0 || inventory["omitted_count"] != float64(1) {
		t.Fatalf("inventory=%#v snapshots=%#v", inventory, snapshots)
	}
}

// The 16 KiB body cap cuts on a character boundary.
func TestObserveSkillsSnapshotCapKeepsUTF8Valid(t *testing.T) {
	l := newSkillLayout(t)
	writeSkillFile(t, filepath.Join(l.home, ".claude", "skills", "u", "SKILL.md"), strings.Repeat("a", maxSnapshotBodyBytes-1)+"é tail")
	_, snapshots, _ := observeClaudeSkills(t, l, "user_claude")
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	body := snapshots[0]["snapshot"].(string)
	if !utf8.ValidString(body) || body != strings.Repeat("a", maxSnapshotBodyBytes-1) || snapshots[0]["truncated"] != true {
		t.Fatalf("body = %d bytes, valid=%v", len(body), utf8.ValidString(body))
	}
}
