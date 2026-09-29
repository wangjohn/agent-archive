package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentcommands"
	"github.com/wangjohn/agent-archive/internal/config"
)

func claudeSkillPath(userHome string) string {
	return filepath.Join(userHome, ".claude", "skills", "handoff", "SKILL.md")
}

func agentsSkillPath(userHome string) string {
	return filepath.Join(userHome, ".agents", "skills", "handoff", "SKILL.md")
}

func uninstallRun(t *testing.T, env Env) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall exit %d\n%s\n%s", code, &out, &errOut)
	}
	return out.String()
}

func TestSetupInstallsTheHandoffSkillUnderTheSandboxedHome(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
	cfg, _, err := config.Load(home)
	must(t, err)
	files := agentcommands.Files(userHome, filepath.Join(userHome, ".claude"), []string{"codex", "claude"}, cfg.InstalledExecutable)
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil || !bytes.Equal(data, f.Content) {
			t.Fatalf("%s not installed: %v", f.Path, err)
		}
	}
	if !strings.Contains(output, "Installed /handoff, which continues a session in another agent: ~/.claude/skills/handoff/SKILL.md, ~/.agents/skills/handoff/SKILL.md\n") {
		t.Fatalf("setup did not say where /handoff went:\n%s", output)
	}

	commands := statusJSON(t, env)["agent_commands"]
	if !reflect.DeepEqual(commands, []any{claudeSkillPath(userHome), agentsSkillPath(userHome)}) {
		t.Fatalf("status agent_commands = %#v", commands)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "  /handoff:      ~/.claude/skills/handoff/SKILL.md\n") {
		t.Fatalf("status --verbose lacks the skill:\n%s", &out)
	}

	uninstallRun(t, env)
	for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s", dir)
		}
	}
}

func TestSetupLeavesAHandoffSkillItDidNotWrite(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	path := agentsSkillPath(userHome)
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte("my own skill\n"), 0600))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, true, project), 0)
	if !strings.Contains(output, "Left ~/.agents/skills/handoff/SKILL.md as it is: it lacks agent-archive's marker line, so /handoff is not installed there.\n") {
		t.Fatalf("setup did not report the file it left:\n%s", output)
	}
	if strings.Contains(output, "Installed /handoff") {
		t.Fatalf("setup claims an installation:\n%s", output)
	}
	if _, ok := statusJSON(t, env)["agent_commands"]; ok {
		t.Fatal("status lists a file setup did not write")
	}
	if output := uninstallRun(t, env); !strings.Contains(output, "Kept ~/.agents/skills/handoff/SKILL.md: it lacks agent-archive's marker line, so it is yours.\n") {
		t.Fatalf("uninstall did not report the file it kept:\n%s", output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "my own skill\n" {
		t.Fatalf("the person's skill changed: %q %v", data, err)
	}
}

// A skill whose marker line the person deleted is theirs: setup and
// uninstall leave it, and status no longer lists it.
func TestSetupAndUninstallKeepAHandoffSkillMadeOwn(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", false, true, false, t.TempDir()))
	path := claudeSkillPath(userHome)
	must(t, os.WriteFile(path, []byte("tuned by hand\n"), 0600))
	if _, ok := statusJSON(t, env)["agent_commands"]; ok {
		t.Fatal("status lists a skill that is no longer setup's")
	}
	old, _, err := config.Load(home)
	must(t, err)
	next := old
	must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
	output := uninstallRun(t, env)
	if !strings.Contains(output, "Kept ~/.claude/skills/handoff/SKILL.md: it lacks agent-archive's marker line, so it is yours.\n") {
		t.Fatalf("uninstall did not report the kept file:\n%s", output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "tuned by hand\n" {
		t.Fatalf("edited skill changed: %q %v", data, err)
	}
}

// Setup run again writes nothing new while the files are as it left them,
// and removes the shared skill once no app that reads it is set up.
func TestSetupAgainUpdatesOnlyItsOwnHandoffSkills(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	old, _, err := config.Load(home)
	must(t, err)
	exe, err := env.executable()
	must(t, err)
	before, err := os.Stat(claudeSkillPath(userHome))
	must(t, err)

	next := old
	must(t, applySetup(home, userHome, exe, old, &next, nil, env))
	again, _, err := config.Load(home)
	must(t, err)
	if after, err := os.Stat(claudeSkillPath(userHome)); err != nil || !os.SameFile(before, after) {
		t.Fatal("an unchanged skill was rewritten")
	}

	next = again
	next.Harnesses = []string{"claude"}
	must(t, applySetup(home, userHome, exe, again, &next, nil, env))
	if _, err := os.Stat(filepath.Join(userHome, ".agents")); !os.IsNotExist(err) {
		t.Fatal("the shared skill, or directories setup created for it, stayed after Codex was removed")
	}
	if _, err := os.Stat(claudeSkillPath(userHome)); err != nil {
		t.Fatal("Claude Code's skill removed")
	}
}

func TestFailedSetupTakesBackTheHandoffSkill(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LoadLaunchAgent = func(string) error { return errors.New("bootstrap failed") }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 1)
	for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("failed setup left %s", dir)
		}
	}
}

// TestAgentCommandsSpelling pins the status --json key.
func TestAgentCommandsSpelling(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(statusView{AgentCommands: []string{"/p"}})
	must(t, err)
	if !strings.Contains(string(data), `"agent_commands":["/p"]`) {
		t.Errorf("status = %s", data)
	}
}
