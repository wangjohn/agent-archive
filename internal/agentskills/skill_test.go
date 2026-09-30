package agentskills

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// /handoff's rendered files are pinned byte for byte. The files in
// testdata/handoff were rendered by the Go string literals this package
// started with (handoff v2 package F, #152) before the text moved into
// skills/handoff/SKILL.md.tmpl, so this test also proves that move changed
// no byte. A change to the text is a change to what every installed skill
// says: review the golden diff.
func TestHandoffRendersByteForByte(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		executable string
		dataHome   string
	}{
		{"plain", exe, ""},
		{"quoted-path", "/Users/me/My Tools/agent-archive", ""},
		{"data-home", exe, "/tmp/test home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := skillFiles(handoffOnly, "/Users/me", claudeDir("/Users/me"), []string{"claude", "codex"}, tc.executable, tc.dataHome)
			if len(files) != 2 || files[0].Skill != "handoff" || files[1].Skill != "handoff" {
				t.Fatalf("files = %+v", files)
			}
			golden.Check(t, filepath.Join("testdata", "handoff", "claude-"+tc.name+".md"), files[0].Content)
			golden.Check(t, filepath.Join("testdata", "handoff", "shared-"+tc.name+".md"), files[1].Content)
		})
	}
}

// Every skill in the Registry is a well-formed skill file for both
// destinations, for any executable: its directory name is its name, it
// carries the marker line that makes it setup's, it names the command it
// runs, and it is not the one of another skill.
func TestRegistrySkillsAreWellFormed(t *testing.T) {
	t.Parallel()
	if len(Registry) == 0 {
		t.Fatal("no skills registered")
	}
	names := map[string]bool{}
	for _, s := range Registry {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(s.Name) || names[s.Name] {
			t.Errorf("skill name %q is malformed or repeated", s.Name)
		}
		names[s.Name] = true
		for _, dest := range []Destination{Claude, Shared} {
			for _, dataHome := range []string{"", "/tmp/test home"} {
				content := string(s.Render(dest, exe, dataHome))
				if !strings.HasPrefix(content, "---\nname: "+s.Name+"\n") {
					t.Errorf("%s (%d): frontmatter does not name the skill:\n%s", s.Name, dest, content)
				}
				if !owned([]byte(content), dataHome) {
					t.Errorf("%s (%d, data home %q) is not owned by the installation that rendered it:\n%s", s.Name, dest, dataHome, content)
				}
				if !strings.Contains(content, commandLine(exe, dataHome)) {
					t.Errorf("%s (%d) never names its command:\n%s", s.Name, dest, content)
				}
			}
			// An executable that is not recorded yet renders without a panic.
			s.Render(dest, "", "")
		}
	}
}

// Codex and Cursor read one shared file, so it may not carry a field only
// Claude Code understands.
func TestSharedFileHasNoClaudeOnlyFrontmatter(t *testing.T) {
	t.Parallel()
	for _, s := range Registry {
		content := string(s.Render(Shared, exe, ""))
		front, _, _ := strings.Cut(strings.TrimPrefix(content, "---\n"), "\n---\n")
		for line := range strings.SplitSeq(front, "\n") {
			key, _, _ := strings.Cut(line, ":")
			if slices.Contains([]string{"allowed-tools", "disable-model-invocation", "argument-hint"}, key) {
				t.Errorf("%s: shared frontmatter has %q", s.Name, line)
			}
		}
	}
}
