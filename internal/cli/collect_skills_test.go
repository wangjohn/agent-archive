package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Today's skills attached to a session that ran before them would be false
// evidence, so an import gets no skill observation.
func TestSkillObserverSkipsImports(t *testing.T) {
	t.Parallel()
	userHome, project := t.TempDir(), t.TempDir()
	skill := filepath.Join(userHome, ".claude", "skills", "review")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: review\n---\nsynthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, t.TempDir(), time.Now())
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	observe := skillObserver(env)
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: "claude"}, ProjectRoot: project}
	hook, err := observe(reg, time.Now())
	if err != nil || len(hook) == 0 {
		t.Fatalf("test precondition: a hook session observes the skill: %#v %v", hook, err)
	}
	reg.Origin = archive.SessionOriginImport
	if imported, err := observe(reg, time.Now()); err != nil || imported != nil {
		t.Fatalf("an import was given today's skills: %#v %v", imported, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSkillObserverReadsUserScopeOncePerHarnessAndProjectScopePerProject(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	projectA, projectB := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	userRoot := filepath.Join(userHome, ".claude", "skills")
	write(userRoot, "shared", "v1")
	write(filepath.Join(projectA, ".claude", "skills"), "alpha", "a")
	write(filepath.Join(projectB, ".claude", "skills"), "beta", "b")
	env := testEnv(t, home, time.Now())
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	observe := skillObserver(env)
	harness := archive.Harness{Name: "claude"}
	first, err := observe(archive.SessionRegistration{ProjectRoot: projectA, Harness: harness}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Changing the user-scope skill between sessions of the same pass must
	// not be observed again: the user root is read once per harness.
	write(userRoot, "shared", "v2")
	second, err := observe(archive.SessionRegistration{ProjectRoot: projectB, Harness: harness}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshots := func(items []archive.SupplementalEvidence) map[string]string {
		out := map[string]string{}
		for _, item := range items {
			if item.Kind == archive.EvidenceKindSkillSnapshot {
				out[item.Payload["scope"].(string)+"/"+item.Payload["name"].(string)] = item.Payload["snapshot"].(string)
			}
		}
		return out
	}
	got1, got2 := snapshots(first), snapshots(second)
	if !strings.HasSuffix(got1["user_claude/shared"], "v1") || got1["user_claude/shared"] != got2["user_claude/shared"] {
		t.Fatalf("user scope was re-read per project: first=%v second=%v", got1, got2)
	}
	if got1["project_claude/alpha"] == "" || got1["project_claude/beta"] != "" || got2["project_claude/beta"] == "" || got2["project_claude/alpha"] != "" {
		t.Fatalf("project scope was not keyed per project: first=%v second=%v", got1, got2)
	}
	for _, items := range [][]archive.SupplementalEvidence{first, second} {
		if len(items) < 2 || items[0].Payload["scope"] != "user_claude" || items[len(items)-2].Payload["scope"] != "project_claude" {
			t.Fatalf("scope order changed: %#v", items)
		}
	}
}
