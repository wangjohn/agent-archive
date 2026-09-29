package agentcommands

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

const exe = "/Users/me/bin/agent-archive"

func claudeDir(home string) string { return filepath.Join(home, ".claude") }

func paths(files []File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func TestFilesFollowTheHarnesses(t *testing.T) {
	t.Parallel()
	home := "/Users/me"
	claude := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
	agents := filepath.Join(home, ".agents", "skills", "handoff", "SKILL.md")
	for _, tc := range []struct {
		harnesses []string
		want      []string
	}{
		{nil, nil},
		{[]string{"claude"}, []string{claude}},
		{[]string{"codex"}, []string{agents}},
		{[]string{"cursor"}, []string{agents}},
		{[]string{"codex", "claude", "cursor"}, []string{claude, agents}},
	} {
		if got := paths(Files(home, claudeDir(home), tc.harnesses, exe)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Files(%v) = %v, want %v", tc.harnesses, got, tc.want)
		}
	}
	files := Files(home, claudeDir(home), []string{"cursor", "codex"}, exe)
	if !reflect.DeepEqual(files[0].Harnesses, []string{"codex", "cursor"}) {
		t.Errorf("shared file harnesses = %v", files[0].Harnesses)
	}
}

func TestSkillContent(t *testing.T) {
	t.Parallel()
	files := Files("/Users/me", claudeDir("/Users/me"), []string{"claude", "codex"}, exe)
	claude, agents := string(files[0].Content), string(files[1].Content)
	for _, want := range []string{
		"---\nname: handoff\n",
		"disable-model-invocation: true\n",
		"allowed-tools: Bash(" + exe + " handoff:*)\n",
		`argument-hint: "[claude|codex|cursor]"`,
		"$ARGUMENTS",
		"    " + exe + " handoff --to <agent>\n",
		marker,
	} {
		if !strings.Contains(claude, want) {
			t.Errorf("Claude Code skill lacks %q:\n%s", want, claude)
		}
	}
	for _, want := range []string{"---\nname: handoff\ndescription: ", "    " + exe + " handoff --to <agent>\n", "codex if you are Claude Code", marker} {
		if !strings.Contains(agents, want) {
			t.Errorf("shared skill lacks %q:\n%s", want, agents)
		}
	}
	for _, unwanted := range []string{"$ARGUMENTS", "allowed-tools", "disable-model-invocation"} {
		if strings.Contains(agents, unwanted) {
			t.Errorf("shared skill has %q", unwanted)
		}
	}
}

// A path the shell would split is quoted in the command, and then no
// permission rule is written: it would not match the command as run.
func TestSkillQuotesAPathThatNeedsIt(t *testing.T) {
	t.Parallel()
	content := string(Files("/Users/me", claudeDir("/Users/me"), []string{"claude"}, "/Users/me/My Tools/agent-archive")[0].Content)
	if !strings.Contains(content, "    '/Users/me/My Tools/agent-archive' handoff --to <agent>\n") {
		t.Errorf("command not quoted:\n%s", content)
	}
	if strings.Contains(content, "allowed-tools") {
		t.Errorf("permission rule for a quoted path:\n%s", content)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPlanInstallWritesThenLeavesTheFilesAsTheyAre(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, foreign, err := PlanInstall(home, claudeDir(home), []string{"claude", "codex"}, exe, claudeDir(home))
	must(t, err)
	if len(changes) != 2 || len(foreign) != 0 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	must(t, hooks.Apply(changes))
	for _, f := range Files(home, claudeDir(home), []string{"claude", "codex"}, exe) {
		if readFile(t, f.Path) != string(f.Content) {
			t.Fatalf("%s not written", f.Path)
		}
	}
	// Reinstalling the same files changes nothing.
	changes, foreign, err = PlanInstall(home, claudeDir(home), []string{"claude", "codex"}, exe, claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 0 {
		t.Fatalf("reinstall: changes=%+v foreign=%v", changes, foreign)
	}
}

func TestPlanInstallUpdatesOnlySetupsFiles(t *testing.T) {
	t.Parallel()
	render := func(executable string) string {
		return string(Files("/h", claudeDir("/h"), []string{"claude"}, executable)[0].Content)
	}
	unmarked := func(content string) string { return strings.Replace(content, marker+"\n", "", 1) }
	for _, tc := range []struct {
		name, content string
		owned         bool
	}{
		{"moved executable", render("/old/agent-archive"), true},
		{"earlier release", "older wording\n" + marker + "\n", true},
		{"the person's own", "my own handoff\n", false},
		{"edited with the marker line deleted", unmarked(render(exe)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
			write(t, path, tc.content)
			changes, foreign, err := PlanInstall(home, claudeDir(home), []string{"claude"}, exe, claudeDir(home))
			must(t, err)
			if tc.owned {
				if len(changes) != 1 || len(foreign) != 0 {
					t.Fatalf("changes=%+v foreign=%v", changes, foreign)
				}
				must(t, hooks.Apply(changes))
				if readFile(t, path) != string(Files(home, claudeDir(home), []string{"claude"}, exe)[0].Content) {
					t.Fatal("setup's file not updated")
				}
				return
			}
			if len(changes) != 0 || !reflect.DeepEqual(foreign, []string{path}) {
				t.Fatalf("changes=%+v foreign=%v", changes, foreign)
			}
		})
	}
}

func TestPlanInstallLeavesLinksAndDirectories(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	files := Files(home, claudeDir(home), []string{"claude", "codex"}, exe)
	target := filepath.Join(home, "dotfiles", "SKILL.md")
	write(t, target, string(files[0].Content))
	must(t, os.MkdirAll(filepath.Dir(files[0].Path), 0700))
	must(t, os.Symlink(target, files[0].Path))
	must(t, os.MkdirAll(files[1].Path, 0700))
	changes, foreign, err := PlanInstall(home, claudeDir(home), []string{"claude", "codex"}, exe, claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 2 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
}

func TestPlanInstallRemovesAFileNoLongerWanted(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := PlanInstall(home, claudeDir(home), []string{"claude", "codex"}, exe, claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	// Codex is no longer set up: its file goes, and then its directories.
	changes, _, err = PlanInstall(home, claudeDir(home), []string{"claude"}, exe, claudeDir(home))
	must(t, err)
	if len(changes) != 1 || !changes[0].Delete {
		t.Fatalf("changes = %+v", changes)
	}
	must(t, hooks.Apply(changes))
	RemoveEmptyDirs(home, claudeDir(home))
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatal("the shared skill or its directories stayed")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")); err != nil {
		t.Fatal("Claude Code's skill removed")
	}
	// A file of the person's own for an app not set up is not setup's concern.
	write(t, filepath.Join(home, ".agents", "skills", "handoff", "SKILL.md"), "mine\n")
	changes, foreign, err := PlanInstall(home, claudeDir(home), []string{"claude"}, exe, claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 0 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
}

func TestPlanRemovalKeepsWhatSetupDidNotWrite(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := PlanInstall(home, claudeDir(home), []string{"claude", "codex"}, exe, claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	files := Files(home, claudeDir(home), []string{"claude", "codex"}, exe)
	write(t, files[1].Path, "edited\n")
	if got := Installed(home, claudeDir(home)); !reflect.DeepEqual(got, []string{files[0].Path}) {
		t.Fatalf("Installed = %v", got)
	}
	changes, kept, err := PlanRemoval(home, claudeDir(home))
	must(t, err)
	if len(changes) != 1 || changes[0].Path != files[0].Path || !changes[0].Delete {
		t.Fatalf("changes = %+v", changes)
	}
	if !reflect.DeepEqual(kept, []string{files[1].Path}) {
		t.Fatalf("kept = %v", kept)
	}
	must(t, hooks.Apply(changes))
	RemoveEmptyDirs(home, claudeDir(home))
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("empty directories kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err != nil {
		t.Fatal("Claude Code's configuration directory removed")
	}
	if readFile(t, files[1].Path) != "edited\n" {
		t.Fatal("edited file changed")
	}
}

// A directory holding something else stays, and so does every parent
// above it; the home folder itself is never removed.
func TestRemoveEmptyDirsStopsAtOneInUse(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, ".claude", "skills", "handoff"), 0700))
	write(t, filepath.Join(home, ".claude", "skills", "other", "SKILL.md"), "x")
	RemoveEmptyDirs(home, claudeDir(home))
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "handoff")); !os.IsNotExist(err) {
		t.Fatal("empty directory kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatal("directory in use removed")
	}
	empty := t.TempDir()
	RemoveEmptyDirs(empty, claudeDir(empty))
	if _, err := os.Stat(empty); err != nil {
		t.Fatal("home folder removed")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
