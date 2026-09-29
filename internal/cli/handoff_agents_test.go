package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeLaunchEnv struct {
	onPath map[string]bool
	env    []string
}

func (f fakeLaunchEnv) lookPath(name string) (string, error) {
	if f.onPath[name] {
		return "/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f fakeLaunchEnv) environ() []string { return f.env }

func TestBuildLaunchSpecPerAgent(t *testing.T) {
	t.Parallel()
	env := fakeLaunchEnv{onPath: map[string]bool{"claude": true, "codex": true, "agent": true, "cursor-agent": true}}
	const file, dir = "/data/handoffs/launch-s-1/handoff.md", "/src/app"
	extra := []string{"--model", "x"}
	for _, tc := range []struct {
		dest   handoffDestination
		binary string
		args   []string
	}{
		// Claude reads outside its project only from an added directory.
		{handoffDestinationClaude, "/bin/claude", []string{"--add-dir", "/data/handoffs/launch-s-1", "--model", "x", "--", "PROMPT"}},
		{handoffDestinationCodex, "/bin/codex", []string{"--cd", dir, "--model", "x", "--", "PROMPT"}},
		{handoffDestinationCursor, "/bin/agent", []string{"--workspace", dir, "--model", "x", "PROMPT"}},
	} {
		spec, err := buildLaunchSpec(tc.dest, "PROMPT", file, dir, extra, env)
		if err != nil {
			t.Fatalf("%s: %v", tc.dest, err)
		}
		if spec.Destination != tc.dest || spec.Binary != tc.binary || spec.Dir != dir || spec.HandoffFile != file || !slices.Equal(spec.Args, tc.args) {
			t.Errorf("%s: spec = %+v, want %s %q", tc.dest, spec, tc.binary, tc.args)
		}
	}
}

func TestBuildLaunchSpecCursorFallsBackToCursorAgent(t *testing.T) {
	t.Parallel()
	spec, err := buildLaunchSpec(handoffDestinationCursor, "P", "/h/f.md", "/d", nil, fakeLaunchEnv{onPath: map[string]bool{"cursor-agent": true}})
	if err != nil || spec.Binary != "/bin/cursor-agent" || !slices.Equal(spec.Args, []string{"--workspace", "/d", "P"}) {
		t.Fatalf("spec=%+v err=%v", spec, err)
	}
}

func TestBuildLaunchSpecMissingBinary(t *testing.T) {
	t.Parallel()
	for dest, want := range map[handoffDestination]string{
		handoffDestinationClaude: "could not find claude: install Claude Code or put it on PATH",
		handoffDestinationCodex:  "could not find codex: install Codex or put it on PATH",
		handoffDestinationCursor: "could not find agent or cursor-agent: install Cursor's CLI or put it on PATH",
	} {
		if _, err := buildLaunchSpec(dest, "P", "/h/f.md", "/d", nil, fakeLaunchEnv{}); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", dest, err, want)
		}
	}
}

// Only the listed session variables go; settings sharing their prefixes
// stay.
func TestBuildLaunchSpecStripsCallingSessionOnly(t *testing.T) {
	t.Parallel()
	var environ []string
	for _, name := range handoffSessionEnv {
		environ = append(environ, name+"=x")
	}
	kept := []string{
		"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=8000", "CLAUDE_CONFIG_DIR=/c",
		"CODEX_HOME=/codex", "CURSOR_API_KEY=k", "ANTHROPIC_API_KEY=a", "EMPTY=", "CLAUDECODE_EXTRA=1",
	}
	environ = append(environ, kept...)
	spec, err := buildLaunchSpec(handoffDestinationCodex, "P", "/h/f.md", "/d", nil, fakeLaunchEnv{onPath: map[string]bool{"codex": true}, env: environ})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Env, kept) {
		t.Fatalf("env = %q, want %q", spec.Env, kept)
	}
}

// The list is what package D also unsets in a new window. It covers every
// variable handoff itself reads to find the calling session; the rest are
// pinned so a removal is deliberate.
func TestHandoffSessionEnvIsPinned(t *testing.T) {
	t.Parallel()
	want := strings.Fields("CLAUDECODE CLAUDE_CODE_SESSION_ID "+
		"CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_CHILD_SESSION CLAUDE_CODE_SESSION_ATTENDED CLAUDE_CODE_EXECPATH "+
		"CLAUDE_CODE_MESSAGING_SOCKET CLAUDE_CODE_MESSAGING_TOKEN CLAUDE_CODE_HOST_SESSION_ID CLAUDE_PID "+
		"CLAUDE_EFFORT AI_AGENT CODEX_THREAD_ID CODEX_SESSION_ID CODEX_CI CODEX_SANDBOX "+
		"CODEX_SANDBOX_NETWORK_DISABLED CODEX_PERMISSION_PROFILE CODEX_VERSION CURSOR_AGENT")
	for _, v := range currentSessionEnv {
		want = append(want, v.key)
	}
	for _, name := range want {
		if !slices.Contains(handoffSessionEnv, name) {
			t.Errorf("handoffSessionEnv lacks %s", name)
		}
	}
}

// Everything after the first `--` is the agent's, even words that look like
// a session ID or this command's own flags.
func TestHandoffOptionsPassArgumentsAfterDoubleDash(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, tc := range []struct {
		args []string
		id   string
		want []string
	}{
		{[]string{"abc", "--to", "codex", "--", "--model", "o3", "--", "abc"}, "abc", []string{"--model", "o3", "--", "abc"}},
		{[]string{"--to", "claude", "abc", "--", "--latest"}, "abc", []string{"--latest"}},
		{[]string{"abc", "--to", "codex", "--"}, "abc", nil},
	} {
		var errOut bytes.Buffer
		opts, ok := parseHandoffOptions(tc.args, &errOut, env, false)
		if !ok || opts.sessionID != tc.id || !slices.Equal(opts.agentArgs, tc.want) {
			t.Errorf("%q: ok=%v id=%q agentArgs=%q stderr=%s", tc.args, ok, opts.sessionID, opts.agentArgs, errOut.String())
		}
	}
}

// A second `--` among the agent's arguments would put the words after it
// ahead of the prompt as positional arguments, so it is refused.
func TestBuildLaunchSpecRefusesDoubleDashInArguments(t *testing.T) {
	t.Parallel()
	env := fakeLaunchEnv{onPath: map[string]bool{"claude": true, "codex": true, "agent": true}}
	for _, dest := range []handoffDestination{handoffDestinationClaude, handoffDestinationCodex, handoffDestinationCursor} {
		_, err := buildLaunchSpec(dest, "P", "/h/f.md", "/d", []string{"--model", "o3", "--", "other prompt"}, env)
		if err == nil || !strings.Contains(err.Error(), "cannot include `--`") {
			t.Errorf("%s: err = %v", dest, err)
		}
	}
}
