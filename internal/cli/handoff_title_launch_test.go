package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// launchRecorder makes handoff's git the real one on a repository at the
// fixture's project, and records the launch a run makes.
func launchRecorder(t *testing.T, f *pickerFixture) *launchSpec {
	t.Helper()
	initTestRepo(t, f.project)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.RunGit = testGit
	// On a terminal, so the agent runs here rather than in a new window.
	f.env.IsTerminal = func(any) bool { return true }
	got := new(launchSpec)
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		*got = spec
		data, err := os.ReadFile(spec.HandoffFile)
		if err != nil {
			t.Fatal(err)
		}
		got.HandoffFile = string(data)
		return nil
	}
	return got
}

// A title with --to is one step: the match is launched, with the document of
// the matched session, whether the match is on this machine or only archived.
func TestHandoffTitleWithToLaunchesTheMatchedSession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	got := launchRecorder(t, &f)
	_, errOut, code := runHandoff(t, f.env, "not uploaded", "--to", "claude")
	if code != 0 || got.Dir != f.project || !strings.Contains(got.HandoffFile, "Not uploaded yet") || !strings.Contains(errOut, "launching local claude") {
		t.Fatalf("code=%d launch=%+v stderr=%s", code, got, errOut)
	}
	_, errOut, code = runHandoff(t, f.env, "archived elsewhere", "--to", "codex")
	if code != 0 || !strings.Contains(got.HandoffFile, "Archived elsewhere") || !strings.Contains(got.HandoffFile, "source: archive") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// A title with --worktree launches in a worktree named for the matched
// session, not for the query.
func TestHandoffTitleWithWorktreeLaunchesInAWorktreeOfTheMatch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	got := launchRecorder(t, &f)
	_, errOut, code := runHandoff(t, f.env, "not uploaded", "--to", "claude", "--worktree", "--branch", "try/title")
	want := resolvedPath(t, f.project) + "-handoff-" + handoffShortID(handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.notUploaded}})
	if code != 0 || got.Dir != want || !strings.Contains(errOut, "on branch try/title") || !strings.Contains(got.HandoffFile, "Not uploaded yet") {
		t.Fatalf("code=%d dir=%q stderr=%s, want %s", code, got.Dir, errOut, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
	// The default branch is named for the session too.
	_, errOut, code = runHandoff(t, f.env, "archived elsewhere", "--to", "codex", "--worktree")
	wantArchived := resolvedPath(t, f.project) + "-handoff-" + handoffShortID(handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.archiveOnly}})
	if code != 0 || got.Dir != wantArchived || !strings.Contains(errOut, "on branch handoff/"+f.archiveOnly[:8]) {
		t.Fatalf("code=%d dir=%q stderr=%s, want %s", code, got.Dir, errOut, wantArchived)
	}
}

// Several matches launch nothing and create no worktree, on a terminal or off.
func TestHandoffTitleAmbiguousWithToLaunchesNothing(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	got := launchRecorder(t, &f)
	f.addSession(t, "claude", "claude-yet", "Not started yet", f.env.now().Add(3*3600e9))
	// Off a terminal.
	f.env.IsTerminal = func(any) bool { return false }
	_, errOut, code := runHandoff(t, f.env, "yet", "--to", "codex", "--worktree")
	if code != 1 || !strings.Contains(errOut, `"yet" matches 2 sessions`) || got.Dir != "" {
		t.Fatalf("code=%d launch=%+v stderr=%s", code, got, errOut)
	}
	// On a terminal, quitting the picker launches nothing either.
	f.env.IsTerminal = func(any) bool { return true }
	_, errOut, code = runHandoff(t, f.env, "yet", "--to", "codex", "--worktree")
	if got.Dir != "" {
		t.Fatalf("code=%d launched %+v after no answer; stderr=%s", code, got, errOut)
	}
	matches, err := filepath.Glob(f.project + "-handoff-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("worktrees created: %v %v", matches, err)
	}
}

// Inside a coding agent nothing is asked, even on a terminal: several matches
// are listed, not offered in a picker.
func TestHandoffTitleAmbiguousInsideAnAgentNeverPrompts(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addSession(t, "claude", "claude-yet", "Not started yet", f.env.now().Add(3*3600e9))
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "some-other-session"})
	out, errOut, code := runPicker(t, f.env, "1\n", "yet")
	if code != 1 || strings.Contains(out, "Enter number") || !strings.Contains(errOut, `"yet" matches 2 sessions`) {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
}
