package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

const widgetOrigin = "https://example.test/acme/widget.git"

// claudeHandoffTranscript is a Claude Code session on the branch
// fix/widget-test.
const claudeHandoffTranscript = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"native-1","cwd":"PROJECT","gitBranch":"fix/widget-test","version":"2.1.280","timestamp":"2026-01-02T00:00:00Z","origin":{"kind":"human"},"promptSource":"sdk","message":{"role":"user","content":"Fix the flaky widget test."}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"native-1","cwd":"PROJECT","gitBranch":"fix/widget-test","version":"2.1.280","timestamp":"2026-01-02T00:00:02Z","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Next I will inject a fake clock."}]}}
`

// repoFixture is a session captured in a repository whose origin is
// widgetOrigin, published to the archive.
func newRepoFixture(t *testing.T, harness, transcript string) handoffFixture {
	t.Helper()
	f := newHandoffFixtureFor(t, false, harness, transcript)
	f.env.repoKey = func(string) string { return archive.RepoKey(widgetOrigin) }
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, f.env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, errOut.String())
	}
	return f
}

// otherMachine turns f into a second computer that has the archive but not
// the session: no registrations, another machine ID, and the working
// directory a different checkout, returned. Its checkout has key.
func otherMachine(t *testing.T, f *handoffFixture, key string) string {
	t.Helper()
	regs, err := os.ReadDir(filepath.Join(f.home, "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range regs {
		if err := os.Remove(filepath.Join(f.home, "registrations", reg.Name())); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, err := config.Load(f.home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MachineID = "machine-2"
	if err := config.Save(f.home, cfg); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return dir, nil }
	f.env.repoKey = func(string) string { return key }
	return dir
}

// launches counts what a run started: an agent here or in a new window.
type launches struct {
	specs   []launchSpec
	windows int
}

func (l *launches) watch(env *Env) {
	env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		l.specs = append(l.specs, spec)
		return nil
	}
	env.OpenTerminal = func(termlaunch.Spec) (string, error) {
		l.windows++
		return "a new window", nil
	}
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
}

func (l *launches) count() int { return len(l.specs) + l.windows }

func TestArchiveHandoffCandidatesMatchByRepositoryOrPath(t *testing.T) {
	t.Parallel()
	key, other := archive.RepoKey(widgetOrigin), archive.RepoKey("https://example.test/acme/other.git")
	sessions := []archive.Metadata{
		{SessionID: "machine-b-same-repo", ProjectID: "project-b", RepoKey: key},
		{SessionID: "machine-a-same-repo", ProjectID: "project-a", RepoKey: key},
		{SessionID: "same-path-other-key", ProjectID: "project-here", RepoKey: other},
		{SessionID: "same-path-no-key", ProjectID: "project-here"},
		{SessionID: "unrelated", ProjectID: "project-z", RepoKey: other},
		{SessionID: "no-key-elsewhere", ProjectID: "project-y"},
	}
	ids := func(got []archive.Metadata) string {
		var out []string
		for _, m := range got {
			out = append(out, m.SessionID)
		}
		return strings.Join(out, ",")
	}
	here := map[string]bool{"project-here": true}
	// Two machines' project IDs, one key: both match, in the order given.
	// A path match stays a match whatever its key, and a path never matches
	// through a key it lacks.
	if got := ids(archiveHandoffCandidates(sessions, here, key, nil)); got != "machine-b-same-repo,machine-a-same-repo,same-path-other-key,same-path-no-key" {
		t.Errorf("with a key: %s", got)
	}
	// No key: exactly the project-ID rule.
	if got := ids(archiveHandoffCandidates(sessions, here, "", nil)); got != "same-path-other-key,same-path-no-key" {
		t.Errorf("without a key: %s", got)
	}
	// A session without a key never matches an empty key.
	if got := ids(archiveHandoffCandidates(sessions, map[string]bool{}, "", nil)); got != "" {
		t.Errorf("no path and no key matched %s", got)
	}
}

// The point of the change: a session captured on another computer, at another
// path, is found by the repository's origin. On a terminal the person is
// shown what matched and agrees before anything is printed.
func TestHandoffLatestFindsAnotherMachinesSessionByRepository(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "claude", claudeHandoffTranscript)
	otherMachine(t, &f, archive.RepoKey(widgetOrigin))
	f.env.currentBranch = func(string) string { return "main" }
	out, errOut, code := runPicker(t, f.env, "y\np\n", "--latest")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{
		"handoff: using claude session " + f.id,
		"(another machine)",
		"handoff: matched by repository (remote origin), not by path",
		"another machine · project " + filepath.Base(f.project) + " · branch fix/widget-test · started 2026-01-02 00:00 UTC",
		"first prompt: Fix the flaky widget test.",
		"Hand off this session? [y/N]",
		"handoff: session was on `fix/widget-test`; you are on `main`",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	for _, want := range []string{"source: archive", "Next I will inject a fake clock.",
		"The recorded directory differs from your current checkout", "Your current checkout is on branch `main`, not the recorded one."} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// The hostile-remote case: a repository that names another repository's origin
// in its own configuration. Nothing of the other session is printed or
// launched until the person has seen what it is and said yes.
func TestHandoffRepositoryMatchAsksBeforeLaunching(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		answer string
		launch bool
	}{
		"default answer is no": {answer: "\n"},
		"no":                   {answer: "n\n"},
		"input ends":           {answer: ""},
		"yes":                  {answer: "y\n", launch: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRepoFixture(t, "codex", handoffTranscript)
			otherMachine(t, &f, archive.RepoKey(widgetOrigin))
			var l launches
			l.watch(&f.env)
			out, errOut, code := runPicker(t, f.env, tc.answer, "--latest", "--to", "claude")
			if !strings.Contains(errOut, "matched by repository (remote origin), not by path") || !strings.Contains(errOut, "another machine") {
				t.Errorf("stderr does not name the match:\n%s", errOut)
			}
			if tc.launch {
				if code != 0 || l.count() != 1 {
					t.Fatalf("code=%d launches=%d stderr=%s", code, l.count(), errOut)
				}
				return
			}
			if code != 1 || l.count() != 0 || out != "" || !strings.Contains(errOut, "canceled; nothing was launched") {
				t.Fatalf("code=%d launches=%d stdout=%q stderr=%s", code, l.count(), out, errOut)
			}
		})
	}
}

// Where nothing can be asked (a pipe, or a coding agent's shell), a match by
// repository alone is never used: it is named without the transcript's words,
// and the command that selects it explicitly is printed.
func TestHandoffRepositoryMatchRefusesWithoutATerminal(t *testing.T) {
	t.Parallel()
	agentMode := agentEnv(map[string]string{envNonInteractive: "1"})
	for name, tc := range map[string]struct {
		args     []string
		terminal bool
		// command is what follows the ID in the command printed.
		command string
	}{
		"piped, printing":               {args: []string{"--latest"}, command: ""},
		"piped, launching":              {args: []string{"--latest", "--to", "codex"}, command: " --to codex"},
		"agent mode, launching":         {args: []string{"--latest", "--to", "codex"}, terminal: true, command: " --to codex"},
		"agent mode, harness and place": {args: []string{"--latest", "--harness", "claude", "--to", "codex", "--worktree"}, terminal: true, command: " --harness claude --to codex --worktree"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRepoFixture(t, "claude", claudeHandoffTranscript)
			otherMachine(t, &f, archive.RepoKey(widgetOrigin))
			var l launches
			l.watch(&f.env)
			var out, errOut bytes.Buffer
			stdin := strings.NewReader("y\n")
			f.env.IsTerminal = func(stream any) bool { return tc.terminal && (stream == any(stdin) || stream == any(&out)) }
			if tc.terminal {
				f.env.LookupEnv = agentMode
			}
			code := Run(append([]string{"handoff"}, tc.args...), stdin, &out, &errOut, f.env)
			if code != 1 || out.Len() != 0 || l.count() != 0 {
				t.Fatalf("code=%d launches=%d stdout=%q stderr=%s", code, l.count(), out.String(), errOut.String())
			}
			stderr := errOut.String()
			for _, want := range []string{"matched by repository (remote origin), not by path", "another machine · project ", "started 2026-01-02 00:00 UTC",
				"run `agent-archive handoff " + f.id + tc.command + "`"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr)
				}
			}
			// What the session said is not shown to a reader that cannot be
			// asked, and neither is where it was.
			for _, leaked := range []string{"Fix the flaky", "fix/widget-test", "first prompt"} {
				if strings.Contains(stderr, leaked) {
					t.Errorf("stderr shows %q without a person to ask:\n%s", leaked, stderr)
				}
			}
		})
	}
}

// Naming the session explicitly is the person's own choice: no key involved,
// no question.
func TestHandoffExplicitSessionIgnoresRepositoryMatching(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, archive.RepoKey(widgetOrigin))
	out, errOut, code := runHandoff(t, f.env, f.id, "--source", "archive")
	if code != 0 || !strings.Contains(out, "Fix the flaky widget test.") || strings.Contains(errOut, "matched by repository") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// A path match is the old behavior exactly: no repository mention, no
// question, even when the session's key is not this checkout's (the remote
// was changed since).
func TestHandoffPathMatchNeedsNoConfirmationWhateverTheKeys(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]string{
		"the same key":       archive.RepoKey(widgetOrigin),
		"a different key":    archive.RepoKey("https://example.test/acme/renamed.git"),
		"no key for the dir": "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRepoFixture(t, "codex", handoffTranscript)
			f.env.repoKey = func(string) string { return key }
			// The archive alone, at the same path.
			out, errOut, code := runHandoff(t, f.env, "--latest", "--source", "archive")
			if code != 0 || !strings.Contains(out, "Fix the flaky widget test.") {
				t.Fatalf("archive: code=%d stderr=%s", code, errOut)
			}
			if strings.Contains(errOut, "repository") {
				t.Errorf("a path match mentioned the repository:\n%s", errOut)
			}
			// This machine's own session.
			out, errOut, code = runHandoff(t, f.env, "--latest")
			if code != 0 || !strings.Contains(out, "source: local") || strings.Contains(errOut, "repository") {
				t.Fatalf("local: code=%d stderr=%s", code, errOut)
			}
		})
	}
}

// A local session at another path but the same origin (a second clone on this
// machine) matches by repository, and is held to the same confirmation.
func TestHandoffLocalSessionInAnotherCloneMatchesByRepository(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	key := archive.RepoKey(widgetOrigin)
	f.env.repoKey = func(string) string { return key }
	// Register the key the way the hook would have.
	regsDir := filepath.Join(f.home, "registrations")
	entries, err := os.ReadDir(regsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("registrations = %v, %v", entries, err)
	}
	path := filepath.Join(regsDir, entries[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	withKey := strings.Replace(string(data), "{", `{"repo_key":"`+key+`",`, 1)
	if err := os.WriteFile(path, []byte(withKey), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env.WorkingDir = func() (string, error) { return t.TempDir(), nil }
	// Without a terminal it refuses; asked, it goes ahead.
	if _, errOut, code := runHandoff(t, f.env, "--latest"); code != 1 || !strings.Contains(errOut, "this machine") || !strings.Contains(errOut, "matched by repository") {
		t.Fatalf("refusal: code=%d stderr=%s", code, errOut)
	}
	out, errOut, code := runPicker(t, f.env, "y\np\n", "--latest")
	if code != 0 || !strings.Contains(out, "source: local") || !strings.Contains(errOut, "matched by repository") {
		t.Fatalf("confirmed: code=%d stderr=%s", code, errOut)
	}
}

// Without a key for the directory (no origin, git missing, not a repository),
// --latest is the old path rule, and the failure says what was tried.
func TestHandoffWithoutAKeyIsTheOldPathRule(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, "")
	out, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	for _, want := range []string{"no session for", "on this machine or in the archive", "Tried this directory's path only",
		"not in a git repository with a remote", "named origin", "Recent archived sessions:", "agent-archive handoff " + f.id} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "project ID") || strings.Contains(errOut, "matched by repository") {
		t.Errorf("stderr still explains the old rule or claims a match:\n%s", errOut)
	}
}

// With a key that matches nothing, the message says the repository was tried
// first, then the path.
func TestHandoffNoMatchNamesTheRepositoryAndPathTried(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, archive.RepoKey("https://example.test/acme/unrelated.git"))
	_, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 1 || !strings.Contains(errOut, "Tried this directory's repository (its remote origin), then its path.") ||
		strings.Contains(errOut, "not in a git repository") || !strings.Contains(errOut, "agent-archive handoff "+f.id) {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// Harness filtering and the calling agent's own session still apply.
func TestHandoffRepositoryMatchHonorsHarnessAndSkipsTheCallingSession(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, archive.RepoKey(widgetOrigin))
	if _, errOut, code := runPicker(t, f.env, "y\np\n", "--latest", "--harness", "claude"); code != 1 || !strings.Contains(errOut, "no session for") {
		t.Fatalf("other harness: code=%d stderr=%s", code, errOut)
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CODEX_THREAD_ID": "native-1"})
	if _, errOut, code := runHandoff(t, f.env, "--latest"); code != 1 || !strings.Contains(errOut, "no session for") {
		t.Fatalf("calling session: code=%d stderr=%s", code, errOut)
	}
}

// Cross-machine launch: the archived session is downloaded, written to a
// private file, and the agent is started on it, with retrieval hints that
// work for a session this machine never had.
func TestHandoffLaunchesAnotherMachinesArchivedSession(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	dir := otherMachine(t, &f, archive.RepoKey(widgetOrigin))
	var l launches
	l.watch(&f.env)
	_, errOut, code := runPicker(t, f.env, "y\n", "--latest", "--to", "claude")
	if code != 0 || len(l.specs) != 1 {
		t.Fatalf("code=%d launches=%+v stderr=%s", code, l, errOut)
	}
	spec := l.specs[0]
	if spec.Dir != dir || spec.Destination != handoffDestinationClaude {
		t.Errorf("spec = %+v, want claude in %s", spec, dir)
	}
	path := handoffPathFromPrompt(t, spec)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("handoff file: info=%v err=%v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)
	for _, want := range []string{
		"show " + f.id + " --harness codex --transcript",
		"handoff " + f.id + " --source archive --harness codex --max-bytes 0",
		"source: archive",
		"Fix the flaky widget test.",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("handoff document missing %q", want)
		}
	}
	if strings.Contains(document, "--source local") || strings.Contains(document, "hunter2secret") {
		t.Errorf("handoff document points at a local transcript or leaks a credential:\n%s", document)
	}
}

// The same session is also handed off by ID with no terminal, to a window.
func TestHandoffLaunchesAnotherMachinesSessionByIDWithoutATerminal(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, "")
	var l launches
	l.watch(&f.env)
	if _, errOut, code := runHandoff(t, f.env, f.id, "--to", "claude"); code != 0 || l.windows != 1 {
		t.Fatalf("code=%d launches=%+v stderr=%s", code, l, errOut)
	}
}

// Running in a subdirectory of the repository, or with --project inside it,
// matches by the repository; a parent of repositories does not. This runs the
// real git, which is what makes a subdirectory answer with its checkout's key.
func TestHandoffRepositoryKeyComesFromTheGitCheckoutOfTheDirectory(t *testing.T) {
	if _, err := testGit(t.Context(), t.TempDir(), "--version"); err != nil {
		t.Skip("git is not installed")
	}
	f := newRepoFixture(t, "codex", handoffTranscript)
	otherMachine(t, &f, "")
	f.env.repoKey = nil
	parent := t.TempDir()
	repo := filepath.Join(parent, "widget")
	sub := filepath.Join(repo, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q")
	// SSH form: the same repository as widgetOrigin's HTTPS.
	mustGit(t, repo, "remote", "add", "origin", "git@example.test:acme/widget.git")
	for _, dir := range []string{repo, sub} {
		f.env.WorkingDir = func() (string, error) { return dir, nil }
		if _, errOut, code := runPicker(t, f.env, "y\np\n", "--latest"); code != 0 || !strings.Contains(errOut, "matched by repository") {
			t.Errorf("from %s: code=%d stderr=%s", dir, code, errOut)
		}
	}
	if _, errOut, code := runPicker(t, f.env, "y\np\n", "--latest", "--project", sub); code != 0 || !strings.Contains(errOut, "matched by repository") {
		t.Errorf("--project: code=%d stderr=%s", code, errOut)
	}
	f.env.WorkingDir = func() (string, error) { return parent, nil }
	if _, errOut, code := runPicker(t, f.env, "y\np\n", "--latest"); code != 1 || !strings.Contains(errOut, "no session for") {
		t.Errorf("from the parent of the repository: code=%d stderr=%s", code, errOut)
	}
}

func TestNoteBranchDifference(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		workspace archive.HandoffWorkspace
		want      string
	}{
		"different":                       {archive.HandoffWorkspace{Branch: "feature/x", CurrentBranch: "main"}, "handoff: session was on `feature/x`; you are on `main`\n"},
		"same":                            {archive.HandoffWorkspace{Branch: "main"}, ""},
		"unknown":                         {archive.HandoffWorkspace{}, ""},
		"a terminal escape in the branch": {archive.HandoffWorkspace{Branch: "a\x1b]52;c;x\x07b", CurrentBranch: "main"}, "handoff: session was on `a]52;c;xb`; you are on `main`\n"},
	} {
		var b bytes.Buffer
		noteBranchDifference(archive.Handoff{Workspace: tc.workspace}, &b)
		if got := b.String(); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
