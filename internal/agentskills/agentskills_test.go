package agentskills

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/skillconfig"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
		if got := paths(skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), tc.harnesses, exe, "")); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Files(builtin.NewBuiltins(), %v) = %v, want %v", tc.harnesses, got, tc.want)
		}
	}
	files := skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"cursor", "codex"}, exe, "")
	if !reflect.DeepEqual(files[0].Harnesses, []string{"codex", "cursor"}) {
		t.Errorf("shared file harnesses = %v", files[0].Harnesses)
	}
}

func TestSkillContent(t *testing.T) {
	t.Parallel()
	files := skillFiles(builtin.NewBuiltins(), handoffOnly, "/Users/me", claudeDir("/Users/me"), []string{"claude", "codex"}, exe, "")
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
	for _, want := range []string{"---\nname: handoff\ndescription: ", "    " + exe + " handoff --to <agent>\n", "codex if you are Claude Code", "Only if they explicitly asked you to hand off", marker} {
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
	content := string(skillFiles(builtin.NewBuiltins(), handoffOnly, "/Users/me", claudeDir("/Users/me"), []string{"claude"}, "/Users/me/My Tools/agent-archive", "")[0].Content)
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
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 2 || len(foreign) != 0 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	must(t, hooks.Apply(changes))
	for _, f := range skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "") {
		if readFile(t, f.Path) != string(f.Content) {
			t.Fatalf("%s not written", f.Path)
		}
	}
	// Reinstalling the same files changes nothing.
	changes, foreign, err = planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 0 {
		t.Fatalf("reinstall: changes=%+v foreign=%v", changes, foreign)
	}
}

func TestPlanInstallUpdatesOnlySetupsFiles(t *testing.T) {
	t.Parallel()
	render := func(executable string) string {
		return string(skillFiles(builtin.NewBuiltins(), handoffOnly, "/h", claudeDir("/h"), []string{"claude"}, executable, "")[0].Content)
	}
	unmarked := func(content string) string { return strings.Replace(content, marker+"\n", "", 1) }
	for _, tc := range []struct {
		name    string
		content string
		owned   bool
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
			changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
			must(t, err)
			if tc.owned {
				if len(changes) != 1 || len(foreign) != 0 {
					t.Fatalf("changes=%+v foreign=%v", changes, foreign)
				}
				must(t, hooks.Apply(changes))
				if readFile(t, path) != string(skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "")[0].Content) {
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
	files := skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "")
	target := filepath.Join(home, "dotfiles", "SKILL.md")
	write(t, target, string(files[0].Content))
	must(t, os.MkdirAll(filepath.Dir(files[0].Path), 0700))
	must(t, os.Symlink(target, files[0].Path))
	must(t, os.MkdirAll(files[1].Path, 0700))
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 2 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
}

// A linked handoff directory is a skill of the person's own: setup
// neither writes into it nor removes or lists what is in it. A linked
// skills directory above it is written through, as hook files are.

func TestLinkedSkillDirectories(t *testing.T) {
	t.Parallel()
	home, mine := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
	must(t, os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0700))
	must(t, os.Symlink(mine, filepath.Dir(path)))
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 || !reflect.DeepEqual(foreign, []string{path}) {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	write(t, filepath.Join(mine, "SKILL.md"), string(skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "")[0].Content))
	changes, kept, err := planRemovalOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), "")
	must(t, err)
	if len(changes) != 0 || !reflect.DeepEqual(kept, []string{path}) {
		t.Fatalf("removal: changes=%+v kept=%v", changes, kept)
	}
	if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), ""); len(got) != 0 {
		t.Fatalf("Installed = %v", got)
	}

	dotfiles := t.TempDir()
	home = t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, ".agents"), 0700))
	must(t, os.Symlink(dotfiles, filepath.Join(home, ".agents", "skills")))
	changes, foreign, err = planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 1 || len(foreign) != 0 {
		t.Fatalf("linked skills: changes=%+v foreign=%v", changes, foreign)
	}
	must(t, hooks.Apply(changes))
	readFile(t, filepath.Join(dotfiles, "handoff", "SKILL.md"))
}

func TestPlanInstallRemovesAFileNoLongerWanted(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	// Codex is no longer set up: its file goes, and then its directories.
	changes, _, err = planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 1 || !changes[0].Delete {
		t.Fatalf("changes = %+v", changes)
	}
	must(t, hooks.Apply(changes))
	removeEmptyDirs(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home))
	if _, err := os.Stat(filepath.Join(home, ".agents")); !os.IsNotExist(err) {
		t.Fatal("the shared skill or its directories stayed")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")); err != nil {
		t.Fatal("Claude Code's skill removed")
	}
	// A file of the person's own for an app not set up is not setup's concern.
	write(t, filepath.Join(home, ".agents", "skills", "handoff", "SKILL.md"), "mine\n")
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 || len(foreign) != 0 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
}

func TestPlanRemovalKeepsWhatSetupDidNotWrite(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	files := skillFiles(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, "")
	write(t, files[1].Path, "edited\n")
	if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), ""); !reflect.DeepEqual(got, []string{files[0].Path}) {
		t.Fatalf("Installed = %v", got)
	}
	changes, kept, err := planRemovalOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), "")
	must(t, err)
	if len(changes) != 1 || changes[0].Path != files[0].Path || !changes[0].Delete {
		t.Fatalf("changes = %+v", changes)
	}
	if !reflect.DeepEqual(kept, []string{files[1].Path}) {
		t.Fatalf("kept = %v", kept)
	}
	must(t, hooks.Apply(changes))
	removeEmptyDirs(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home))
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
	removeEmptyDirs(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home))
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "handoff")); !os.IsNotExist(err) {
		t.Fatal("empty directory kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); err != nil {
		t.Fatal("directory in use removed")
	}
	empty := t.TempDir()
	removeEmptyDirs(builtin.NewBuiltins(), handoffOnly, empty, claudeDir(empty))
	if _, err := os.Stat(empty); err != nil {
		t.Fatal("home folder removed")
	}
}

// A relocated installation's skill runs with its data directory, as its
// hooks do, since the agent's environment need not have it.

func TestSkillNamesARelocatedDataDirectory(t *testing.T) {
	t.Parallel()
	files := skillFiles(builtin.NewBuiltins(), handoffOnly, "/Users/me", claudeDir("/Users/me"), []string{"claude", "codex"}, exe, "/tmp/test home")
	for _, f := range files {
		content := string(f.Content)
		if !strings.Contains(content, "    AGENT_ARCHIVE_HOME='/tmp/test home' "+exe+" handoff --to <agent>\n") {
			t.Errorf("%s lacks the data directory:\n%s", f.Path, content)
		}
		if strings.Contains(content, "allowed-tools") {
			t.Errorf("%s allows a command that is not a plain path:\n%s", f.Path, content)
		}
	}
}

// Each installation sharing a HOME (with hook files of its own) replaces,
// removes, and lists only the skill naming its own data directory.

func TestInstallationsKeepEachOthersSkills(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		writer string
		other  string
	}{{"", "/data/b"}, {"/data/a", ""}, {"/data/a", "/data/b"}} {
		home := t.TempDir()
		changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude", "codex"}, exe, tc.writer, claudeDir(home))
		must(t, err)
		must(t, hooks.Apply(changes))
		changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, "/other/agent-archive", tc.other, claudeDir(home))
		must(t, err)
		if len(changes) != 0 || len(foreign) != 1 {
			t.Errorf("%q over %q: changes=%+v foreign=%v", tc.other, tc.writer, changes, foreign)
		}
		changes, kept, err := planRemovalOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), tc.other)
		must(t, err)
		if len(changes) != 0 || len(kept) != 2 {
			t.Errorf("%q removing %q's: changes=%+v kept=%v", tc.other, tc.writer, changes, kept)
		}
		if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), tc.other); len(got) != 0 {
			t.Errorf("%q lists %q's: %v", tc.other, tc.writer, got)
		}
		if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), tc.writer); len(got) != 2 {
			t.Errorf("%q does not list its own: %v", tc.writer, got)
		}
	}
}

// A linked directory (a dotfile manager's ~/.claude/skills, or a handoff
// skill of the person's own linked in) is never unlinked, even when what
// it names is empty: os.Remove would remove the link itself.

func TestRemoveEmptyDirsKeepsLinks(t *testing.T) {
	t.Parallel()
	home, dotfiles := t.TempDir(), t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dotfiles, "skills", "handoff"), 0700))
	must(t, os.MkdirAll(filepath.Join(home, ".claude"), 0700))
	must(t, os.Symlink(filepath.Join(dotfiles, "skills"), filepath.Join(home, ".claude", "skills")))
	must(t, os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0700))
	must(t, os.Symlink(t.TempDir(), filepath.Join(home, ".agents", "skills", "handoff")))
	removeEmptyDirs(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home))
	for _, link := range []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".agents", "skills", "handoff")} {
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s unlinked: %v", link, err)
		}
	}
}

// A relocated installation's skill stays its own after the executable
// moves (an upgrade), even with a data directory that needs quoting.

func TestRelocatedSkillSurvivesAMovedExecutable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const data = "/data/it's here"
	changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, "/old/agent-archive", data, claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, data, claudeDir(home))
	must(t, err)
	if len(changes) != 1 || len(foreign) != 0 || !strings.Contains(string(changes[0].After), exe) {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), data); len(got) != 1 {
		t.Fatalf("Installed = %v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// testSkill is a skill that exists only in tests, the second entry of a
// registry: its text names the destination and the command it runs.

func testSkill(name string) Skill {
	return Skill{Name: name, Render: func(dest Destination, executable, dataHome string) []byte {
		return []byte("---\nname: " + name + "\n---\n" + marker + "\n" + commandLine(executable, dataHome) + " " + name + " " + strconv.Itoa(int(dest)) + "\n")
	}}
}

// handoffOnly is a registry of just /handoff, for the tests that pin its
// files and how they are installed whatever else is registered.

var handoffOnly = []Skill{handoffSkill}

func twoSkills() []Skill { return []Skill{handoffSkill, testSkill("second")} }

func TestFilesListEachSkillInRegistryOrder(t *testing.T) {
	t.Parallel()
	home := "/Users/me"
	want := func(claude bool, agents bool) []string {
		var out []string
		for _, name := range []string{"handoff", "second"} {
			if claude {
				out = append(out, filepath.Join(home, ".claude", "skills", name, "SKILL.md"))
			}
			if agents {
				out = append(out, filepath.Join(home, ".agents", "skills", name, "SKILL.md"))
			}
		}
		return out
	}
	for _, tc := range []struct {
		harnesses []string
		want      []string
	}{
		{nil, nil},
		{[]string{"claude"}, want(true, false)},
		{[]string{"cursor"}, want(false, true)},
		{[]string{"codex", "claude", "cursor"}, want(true, true)},
	} {
		got := skillFiles(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), tc.harnesses, exe, "")
		if !reflect.DeepEqual(paths(got), tc.want) {
			t.Errorf("skillFiles(builtin.NewBuiltins(), %v) = %v, want %v", tc.harnesses, paths(got), tc.want)
		}
	}
	files := skillFiles(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude", "codex"}, exe, "/tmp/data")
	if len(files) != 4 || files[0].Skill != "handoff" || files[2].Skill != "second" {
		t.Fatalf("files = %+v", files)
	}
	if !strings.Contains(string(files[2].Content), "AGENT_ARCHIVE_HOME=/tmp/data "+exe+" second 0\n") || !strings.Contains(string(files[3].Content), " second 1\n") {
		t.Errorf("second skill rendered %q and %q", files[2].Content, files[3].Content)
	}
}

// Each skill's files are the setup's own or a foreign one independently: a
// file of the person's own for one skill leaves the others installed, and
// uninstalling keeps just that one.

func TestSkillsInstallAndRemoveIndependently(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	foreign := filepath.Join(home, ".agents", "skills", "second", "SKILL.md")
	write(t, foreign, "my own second skill\n")
	changes, foreignPaths, err := planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 3 || !reflect.DeepEqual(foreignPaths, []string{foreign}) {
		t.Fatalf("changes=%+v foreign=%v", changes, foreignPaths)
	}
	must(t, hooks.Apply(changes))
	installed := installedOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "")
	if len(installed) != 3 {
		t.Fatalf("installed = %v", installed)
	}
	// Reinstalling changes nothing more.
	changes, _, err = planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 {
		t.Fatalf("reinstall changes = %+v", changes)
	}
	removals, kept, err := planRemovalOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "")
	must(t, err)
	if len(removals) != 3 || !reflect.DeepEqual(kept, []string{foreign}) {
		t.Fatalf("removals=%+v kept=%v", removals, kept)
	}
	must(t, hooks.Apply(removals))
	removeEmptyDirs(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home))
	if readFile(t, foreign) != "my own second skill\n" {
		t.Fatal("the person's skill changed")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); !os.IsNotExist(err) {
		t.Fatal("empty skill directories kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "handoff")); !os.IsNotExist(err) {
		t.Fatal("handoff's empty directory kept")
	}
	if _, err := os.Stat(filepath.Dir(foreign)); err != nil {
		t.Fatal("a directory holding the person's file removed")
	}
}

// A file no longer wanted for a harness goes for every skill in the
// registry, not only the first.

func TestPlanInstallRemovesEverySkillOfAnAppNoLongerChosen(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	changes, _, err = planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 2 || !changes[0].Delete || !changes[1].Delete {
		t.Fatalf("changes = %+v", changes)
	}
}

// Stale is the skill files setup owns whose content this release would
// render differently: a moved executable, or wording from an earlier release.

func TestStaleReportsOnlyOwnedFilesThatDifferFromThisRender(t *testing.T) {
	t.Parallel()
	render := func(executable string) string {
		return string(skillFiles(builtin.NewBuiltins(), twoSkills(), "/h", claudeDir("/h"), []string{"claude"}, executable, "")[0].Content)
	}
	for _, tc := range []struct {
		name    string
		content string
		stale   bool
	}{
		{"current", render(exe), false},
		{"moved executable", render("/old/agent-archive"), true},
		{"earlier release", "older wording\n" + marker + "\n", true},
		{"the person's own", "my own handoff\n", false},
		{"marker line deleted", strings.Replace(render("/old/agent-archive"), marker+"\n", "", 1), false},
		{"another installation's", strings.Replace(render("/old/agent-archive"), "\n"+marker, "\n"+marker+"\nAGENT_ARCHIVE_HOME=/other x", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			path := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
			write(t, path, tc.content)
			var want []string
			if tc.stale {
				want = []string{path}
			}
			if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, ""); !reflect.DeepEqual(got, want) {
				t.Errorf("Stale = %v, want %v", got, want)
			}
		})
	}
}

// Each skill is judged on its own file, and a stale file is always one
// Installed lists.

func TestStaleNamesTheOutdatedSkillOnly(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude", "codex"}, "/old/agent-archive", "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, ""); len(got) != 4 {
		t.Fatalf("every file written for the old executable is stale, got %v", got)
	}
	// Setup with the new executable refreshes only the handoff skill's files.
	changes, _, err = planInstall(builtin.NewBuiltins(), []Skill{handoffSkill}, home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	want := []string{filepath.Join(home, ".claude", "skills", "second", "SKILL.md"), filepath.Join(home, ".agents", "skills", "second", "SKILL.md")}
	got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, "")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Stale = %v, want %v", got, want)
	}
	installed := installedOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "")
	for _, path := range got {
		if !slices.Contains(installed, path) {
			t.Errorf("stale %s is not installed", path)
		}
	}
	if len(installed) != 4 {
		t.Errorf("installed = %v", installed)
	}
}

// Nothing is stale without a recorded executable to compare with, or where
// there are no files.

func TestStaleWithNothingToCompare(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, ""); got != nil {
		t.Errorf("no files: Stale = %v", got)
	}
	changes, _, err := planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude"}, "/old/agent-archive", "", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "", ""); got != nil {
		t.Errorf("no executable: Stale = %v", got)
	}
}

// A skill of the person's own linked in (its directory a link) is not
// setup's to call out of date, even with the marker line in it.

func TestStaleLeavesALinkedSkillDirectoryAlone(t *testing.T) {
	t.Parallel()
	home, elsewhere := t.TempDir(), t.TempDir()
	write(t, filepath.Join(elsewhere, "SKILL.md"), "older wording\n"+marker+"\n")
	must(t, os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0700))
	must(t, os.Symlink(elsewhere, filepath.Join(home, ".claude", "skills", "handoff")))
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, ""); got != nil {
		t.Errorf("Stale = %v", got)
	}
}

// Stale follows the installation's data directory as Installed does: a
// relocated installation's skill is judged against a render naming its
// directory, and another installation's file is never reported.

func TestStaleFollowsTheDataDirectory(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := planInstall(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), []string{"claude"}, exe, "/data/a", claudeDir(home))
	must(t, err)
	must(t, hooks.Apply(changes))
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), exe, "/data/a"); got != nil {
		t.Errorf("current relocated skills stale: %v", got)
	}
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "/moved/agent-archive", "/data/a"); len(got) != 2 {
		t.Errorf("moved executable, relocated: Stale = %v", got)
	}
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "/moved/agent-archive", "/data/b"); got != nil {
		t.Errorf("another installation's files reported: %v", got)
	}
	if got := staleOf(builtin.NewBuiltins(), twoSkills(), home, claudeDir(home), "/moved/agent-archive", ""); got != nil {
		t.Errorf("the default installation reported a relocated one's files: %v", got)
	}
}

// The exported entry points read the Registry, so what setup installs and
// what status reports agree with the skills registered.

func TestExportedFunctionsUseTheRegistry(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, foreign, err := PlanInstall(builtin.NewBuiltins(), home, claudeDir(home), []string{"claude", "codex"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 2*len(Registry) || len(foreign) != 0 {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	must(t, hooks.Apply(changes))
	if got := Installed(builtin.NewBuiltins(), home, claudeDir(home), ""); len(got) != 2*len(Registry) {
		t.Errorf("Installed = %v", got)
	}
	if got := Stale(builtin.NewBuiltins(), home, claudeDir(home), exe, ""); got != nil {
		t.Errorf("Stale = %v", got)
	}
	if got := Stale(builtin.NewBuiltins(), home, claudeDir(home), "/moved/agent-archive", ""); len(got) != 2*len(Registry) {
		t.Errorf("Stale after a move = %v", got)
	}
}

// A file replaced or removed keeps its permissions in the change, so that a
// rollback puts the file back as it was, mode and all.

func TestChangesCarryTheFilesPermissions(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
	write(t, path, "older wording\n"+marker+"\n")
	must(t, os.Chmod(path, 0640))
	changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 1 || changes[0].Mode != 0640 || !changes[0].Existed {
		t.Fatalf("replacement = %+v", changes)
	}
	removals, _, err := planRemovalOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), "")
	must(t, err)
	if len(removals) != 1 || removals[0].Mode != 0640 || !removals[0].Delete {
		t.Fatalf("removal = %+v", removals)
	}
	// A new file is private.
	fresh := t.TempDir()
	changes, _, err = planInstall(builtin.NewBuiltins(), handoffOnly, fresh, claudeDir(fresh), []string{"claude"}, exe, "", claudeDir(fresh))
	must(t, err)
	if len(changes) != 1 || changes[0].Mode != 0600 {
		t.Fatalf("new file = %+v", changes)
	}
}

// When Claude Code's configuration directory moved since setup last ran,
// the file in the old one goes, and the new one gets its own.

func TestPlanInstallRemovesTheFileInThePreviousClaudeDirectory(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	previous, current := filepath.Join(home, "old-claude"), filepath.Join(home, "new-claude")
	changes, _, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, previous, []string{"claude"}, exe, "", previous)
	must(t, err)
	must(t, hooks.Apply(changes))
	changes, _, err = planInstall(builtin.NewBuiltins(), handoffOnly, home, current, []string{"claude"}, exe, "", previous)
	must(t, err)
	var written, deleted []string
	for _, c := range changes {
		if c.Delete {
			deleted = append(deleted, c.Path)
		} else {
			written = append(written, c.Path)
		}
	}
	if !reflect.DeepEqual(written, []string{filepath.Join(current, "skills", "handoff", "SKILL.md")}) || !reflect.DeepEqual(deleted, []string{filepath.Join(previous, "skills", "handoff", "SKILL.md")}) {
		t.Fatalf("written=%v deleted=%v", written, deleted)
	}
}

// The marker is a line of its own: a file that only mentions it (quotes it
// in a sentence) is the person's.

func TestAFileThatOnlyQuotesTheMarkerIsNotSetups(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "skills", "handoff", "SKILL.md")
	write(t, path, "My own skill. Setup's marker line is: "+marker+" (I removed it).\n")
	changes, foreign, err := planInstall(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), []string{"claude"}, exe, "", claudeDir(home))
	must(t, err)
	if len(changes) != 0 || !reflect.DeepEqual(foreign, []string{path}) {
		t.Fatalf("changes=%+v foreign=%v", changes, foreign)
	}
	if got := installedOf(builtin.NewBuiltins(), handoffOnly, home, claudeDir(home), ""); len(got) != 0 {
		t.Fatalf("Installed = %v", got)
	}
}

type collidingSkillPorts struct{}

func (collidingSkillPorts) SkillAgents() []string { return []string{"plain", "frontmatter"} }

func (collidingSkillPorts) LookupSkills(name string) (agentapi.SkillProvider, bool) {
	return skillconfig.Provider{ManagedSuffix: "shared-skills", Frontmatter: name == "frontmatter"}, true
}

func TestConflictingSkillDestinationsRefuseBeforeWriting(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	changes, _, err := PlanInstall(collidingSkillPorts{}, home, "", []string{"plain", "frontmatter"}, exe, "", "")
	if err == nil || len(changes) > 0 {
		t.Fatalf("conflicting content produced changes: %v %v", changes, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("conflicting plan touched host: %v %v", entries, err)
	}
}
