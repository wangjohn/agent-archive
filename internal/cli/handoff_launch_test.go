package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// handoffPathFromPrompt reads the handoff document's path out of the
// prompt, the last argument.
func handoffPathFromPrompt(t *testing.T, spec launchSpec) string {
	t.Helper()
	prompt := spec.Args[len(spec.Args)-1]
	const prefix = "Read the complete handoff document at "
	if !strings.HasPrefix(prompt, prefix) || strings.Contains(prompt, "Fix the flaky widget test.") {
		t.Fatalf("prompt contains the transcript or no handoff path: %q", prompt)
	}
	quoted := strings.SplitN(strings.TrimPrefix(prompt, prefix), ", then continue", 2)[0]
	path, err := strconv.Unquote(quoted)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandoffLaunchesLocalAgentWithRetrievalInstructions(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	var got launchSpec
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		got = spec
		return nil
	}
	out, errOut, code := runHandoff(t, f.env, f.id, "--to", "claude")
	if code != 0 || got.Binary == "" || out != "" || !strings.Contains(errOut, "launching local claude") {
		t.Fatalf("code=%d spec=%+v stdout=%q stderr=%q", code, got, out, errOut)
	}
	path := handoffPathFromPrompt(t, got)
	handoffs := filepath.Join(f.home, handoffDir)
	launchDir := filepath.Dir(path)
	if filepath.Dir(launchDir) != handoffs || !strings.HasPrefix(filepath.Base(launchDir), fmt.Sprintf("launch-%s-%d-", f.id, f.env.now().Unix())) {
		t.Fatalf("launch directory %s, want launch-%s-<unix>-<random> in %s", launchDir, f.id, handoffs)
	}
	want := []string{"--add-dir", launchDir, "--", got.Args[3]}
	if got.Destination != handoffDestinationClaude || got.Binary != "/opt/bin/claude" || got.Dir != f.project || !slices.Equal(got.Args, want) {
		t.Fatalf("spec = %+v, want args %q in %s", got, want, f.project)
	}
	// The launch copy persists, alone in its own directory, in the data
	// directory for a resumed session.
	if wantPath := filepath.Join(launchDir, "handoff.md"); path != wantPath || got.HandoffFile != path {
		t.Fatalf("handoff path = %s (spec %s), want %s", path, got.HandoffFile, wantPath)
	}
	if entries, err := os.ReadDir(launchDir); err != nil || len(entries) != 1 {
		t.Fatalf("launch directory holds %v (%v)", entries, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("handoff file after launch: info=%v err=%v", info, err)
	}
	for _, d := range []string{handoffs, launchDir} {
		if dirInfo, err := os.Stat(d); err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s: info=%v err=%v", d, dirInfo, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)
	for _, want := range []string{
		"Agent Archive is available. Its executable is at /opt/agent-archive",
		"show " + f.id + " --harness codex --transcript",
		"handoff " + f.id + " --source local --max-bytes 0",
		"Fix the flaky widget test.",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("handoff document missing %q", want)
		}
	}
	if strings.Contains(document, "hunter2secret") {
		t.Error("handoff document included unfiltered credential")
	}
	if keys, _ := f.mem.List(t.Context(), ""); len(keys) != 0 {
		t.Fatalf("local launch uploaded %d objects", len(keys))
	}
}

func TestHandoffLaunchLatestIncludesCallingSession(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == "CODEX_THREAD_ID" {
			return "native-1", true
		}
		return "", false
	}
	called := false
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, "--latest", "--harness", "codex", "--to", "claude")
	if code != 0 || !called || !strings.Contains(errOut, f.id) {
		t.Fatalf("code=%d called=%v stderr=%q", code, called, errOut)
	}
}

// An agent that exits non-zero may still have a session to resume, which
// reads the same file.
func TestHandoffLaunchFailureKeepsHandoffFile(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	var path string
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		path = handoffPathFromPrompt(t, spec)
		return errors.New("agent failed")
	}
	_, _, code := runHandoff(t, f.env, f.id, "--to", "codex")
	if code != 1 || path == "" {
		t.Fatalf("code=%d path=%q", code, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("handoff removed after the agent failed: %v", err)
	}
}

func TestHandoffLaunchDoesNotFallBackToArchive(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, true)
	regs, err := os.ReadDir(filepath.Join(f.home, "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range regs {
		if err := os.Remove(filepath.Join(f.home, "registrations", reg.Name())); err != nil {
			t.Fatal(err)
		}
	}
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched with no local source")
		return nil
	}
	_, _, code := runHandoff(t, f.env, f.id, "--to", "codex")
	if code == 0 {
		t.Fatal("launched from archive after local source disappeared")
	}
}

// Configured arguments come first, then those after `--`, then the prompt;
// the child environment drops the calling agent's session.
func TestHandoffLaunchPassesConfiguredAndCommandLineArguments(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	cfg, _, err := config.Load(f.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Handoff.Args = map[string][]string{"codex": {"--model", "o3"}, "claude": {"--ignored"}}
	if err := config.Save(f.home, cfg); err != nil {
		t.Fatal(err)
	}
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.Environ = func() []string {
		return []string{"PATH=/opt/bin", "CODEX_THREAD_ID=native-1", "CLAUDE_CODE_USE_BEDROCK=1"}
	}
	var got launchSpec
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		got = spec
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, f.id, "--to", "codex", "--", "--search", "-c", "x=1")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	want := []string{"--cd", f.project, "--model", "o3", "--search", "-c", "x=1", "--"}
	if !slices.Equal(got.Args[:len(got.Args)-1], want) || got.Binary != "/opt/bin/codex" {
		t.Fatalf("args = %q, want %q then the prompt", got.Args, want)
	}
	if !slices.Equal(got.Env, []string{"PATH=/opt/bin", "CLAUDE_CODE_USE_BEDROCK=1"}) {
		t.Fatalf("env = %q", got.Env)
	}
}

func TestHandoffLaunchMissingAgentLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched without an executable")
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, f.id, "--to", "cursor")
	if code != 1 || !strings.Contains(errOut, "could not find agent or cursor-agent: install Cursor's CLI or put it on PATH") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	entries, _ := os.ReadDir(filepath.Join(f.home, handoffDir))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "launch-") {
			t.Errorf("left %s for an agent that never started", entry.Name())
		}
	}
}

// Without setup there is no data directory, so the launch copy goes to a
// private temporary directory. It outlives the agent, since a resumed
// session may read it again; the system clears it.
func TestHandoffLaunchWithoutSetupUsesPrivateTemporaryFile(t *testing.T) {
	t.Parallel()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"user","uuid":"u1","timestamp":"2026-01-02T00:00:00Z","message":{"role":"user","content":"Fix the build."}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "never-set-up")
	env := testEnv(t, home, time.Now())
	env.LookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	env.Environ = func() []string { return nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	env.WorkingDir = func() (string, error) { return filepath.Dir(transcript), nil }
	var path string
	env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		path = handoffPathFromPrompt(t, spec)
		if spec.HandoffFile != path || spec.Args[1] != filepath.Dir(path) || filepath.Dir(filepath.Dir(path)) != env.tempDir() {
			t.Errorf("spec = %+v, want a private directory under %s", spec, env.tempDir())
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("temporary handoff: info=%v err=%v", info, err)
		}
		if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("temporary directory: info=%v err=%v", info, err)
		}
		return nil
	}
	_, errOut, code := runHandoff(t, env, "--file", transcript, "--harness", "claude", "--to", "claude")
	if code != 0 || path == "" {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "Fix the build.") {
		t.Fatalf("private copy after the agent exited: %q %v", data, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("data directory created: %v", err)
	}
}

// Two launches of one session in the same second each get their own
// directory, named with the prefix pruneHandoffs looks for, and neither
// replaces the other's copy. A name already taken, even by a symlink, is
// never reused.
func TestWriteLaunchHandoffSameSecondGetsSeparateDirectories(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	target := handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: "sess-1"}}
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, handoffDir), 0o700); err != nil {
		t.Fatal(err)
	}
	squat := filepath.Join(home, handoffDir, fmt.Sprintf("launch-sess-1-%d", now.Unix()))
	if err := os.Symlink(elsewhere, squat); err != nil {
		t.Fatal(err)
	}
	first, err := writeLaunchHandoff(home, t.TempDir(), target, []byte("first"), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeLaunchHandoff(home, t.TempDir(), target, []byte("second"), now)
	if err != nil {
		t.Fatalf("second launch in the same second: %v", err)
	}
	if filepath.Dir(first) == filepath.Dir(second) {
		t.Fatalf("both launches share %s", filepath.Dir(first))
	}
	for path, want := range map[string]string{first: "first", second: "second"} {
		name := filepath.Base(filepath.Dir(path))
		if !strings.HasPrefix(name, fmt.Sprintf("launch-sess-1-%d-", now.Unix())) || filepath.Dir(filepath.Dir(path)) != filepath.Join(home, handoffDir) {
			t.Errorf("launch copy at %s", path)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Errorf("%s = %q %v, want %q", path, data, err, want)
		}
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}

// Launch copies' directories live beside the untrimmed handoffs, so the
// 7-day prune (by the directory's age) and uninstall's data removal cover
// them. Other directories there are not the prune's to remove.
func TestLaunchHandoffDirectoriesArePrunedAndUninstalled(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	week := now.Add(-8 * 24 * time.Hour)
	target := handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: "sess-1"}}
	old, err := writeLaunchHandoff(home, t.TempDir(), target, []byte("old"), week)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(old), week, week); err != nil {
		t.Fatal(err)
	}
	fresh, err := writeLaunchHandoff(home, t.TempDir(), target, []byte("fresh"), now)
	if err != nil {
		t.Fatal(err)
	}
	if rel, _ := filepath.Rel(home, fresh); !strings.HasPrefix(rel, filepath.Join(handoffDir, fmt.Sprintf("launch-sess-1-%d-", now.Unix()))) || filepath.Base(rel) != "handoff.md" {
		t.Fatalf("launch copy at %s", rel)
	}
	other := filepath.Join(home, handoffDir, "not-a-launch")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, week, week); err != nil {
		t.Fatal(err)
	}
	pruneHandoffs(home, now)
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatalf("week-old launch directory not pruned: %v", err)
	}
	for _, kept := range []string{fresh, other} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s pruned: %v", kept, err)
		}
	}
	leftover, err := removeLocalState(home)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("uninstall leftover=%v err=%v", leftover, err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("launch handoff survived uninstall: %v", err)
	}
}
