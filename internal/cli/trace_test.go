package cli

import (
	"bytes"
	"strings"
	"testing"
)

// withTrace sets AGENT_ARCHIVE_TRACE to value in env, keeping every other
// variable as it was.
func withTrace(env Env, value string) Env {
	previous := env.LookupEnv
	env.LookupEnv = func(key string) (string, bool) {
		if key == envTrace {
			return value, true
		}
		if previous != nil {
			return previous(key)
		}
		return "", false
	}
	return env
}

// With AGENT_ARCHIVE_TRACE on, a command's output is unchanged and stderr
// ends with the timing tree: fixed span names and counts, never a session
// ID or title. Not parallel: the recorder is process-wide.
func TestTraceWritesTheTimingTreeToStderr(t *testing.T) {
	env, _, id := publishedFixture(t)
	var plain, plainErr bytes.Buffer
	if code := Run([]string{"list", "--limit", "0"}, nil, &plain, &plainErr, env); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, plainErr.String())
	}
	if strings.Contains(plainErr.String(), "trace") {
		t.Fatalf("traced without AGENT_ARCHIVE_TRACE:\n%s", plainErr.String())
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--limit", "0"}, nil, &out, &errOut, withTrace(env, "1")); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if out.String() != plain.String() {
		t.Fatalf("tracing changed stdout:\n%s\nwant:\n%s", out.String(), plain.String())
	}
	got := errOut.String()
	for _, want := range []string{"agent-archive trace", "  list  ", "load config", "open store", "list metadata", "list objects", "read sidecars", "sidecars 1", "from cache"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, id) || strings.Contains(got, id[:8]) || strings.Contains(got, "sessions/") {
		t.Errorf("trace names a session or key:\n%s", got)
	}
}

// A value that is not a switch leaves tracing off without failing the
// command, and the hook's internal commands never trace.
func TestTraceOffForInvalidValuesAndInternalCommands(t *testing.T) {
	env, _, _ := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, withTrace(env, "verbose")); code != 0 || strings.Contains(errOut.String(), "trace") {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	errOut.Reset()
	Run([]string{"_collect"}, nil, &out, &errOut, withTrace(env, "1"))
	if strings.Contains(errOut.String(), "agent-archive trace") {
		t.Fatalf("an internal command traced:\n%s", errOut.String())
	}
}

// Only known commands trace, so the root span carries a command's own name,
// never a word from the command line; help and version print no tree.
func TestTraceOnlyKnownCommands(t *testing.T) {
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{{"frobnicate-private-word"}, {"--version"}, {"help"}} {
		var out, errOut bytes.Buffer
		Run(args, nil, &out, &errOut, withTrace(env, "1"))
		if strings.Contains(errOut.String(), "agent-archive trace") {
			t.Fatalf("%v traced:\n%s", args, errOut.String())
		}
	}
}
