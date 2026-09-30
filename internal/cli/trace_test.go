package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
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
	// A lookup that reports the variable unset, whatever value it hands
	// back with that, is off.
	errOut.Reset()
	unset := env
	unset.LookupEnv = func(string) (string, bool) { return "1", false }
	if code := Run([]string{"list"}, nil, &out, &errOut, unset); code != 0 || strings.Contains(errOut.String(), "trace") {
		t.Fatalf("traced with the variable unset: exit=%d stderr=%s", code, errOut.String())
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

// tracedCommands must name exactly the user-facing commands Run dispatches
// (every case but the internal "_" ones, help and version), so a new
// command can't be silently left untraced. Read from Run's own switch.
func TestTracedCommandsMatchRunsSwitch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "cli.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					if !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "-") && name != "help" && name != "version" {
						dispatched[name] = true
					}
				}
			}
			return true
		})
	}
	if len(dispatched) == 0 {
		t.Fatal("found no command cases in Run")
	}
	for name := range dispatched {
		if !tracedCommands[name] {
			t.Errorf("Run dispatches %q but tracedCommands lacks it", name)
		}
	}
	for name := range tracedCommands {
		if !dispatched[name] {
			t.Errorf("tracedCommands has %q, which Run does not dispatch", name)
		}
	}
}

// A handoff that launches an agent in this terminal writes its trace first,
// once, and the agent does not inherit the switch.
func TestTraceFinishesBeforeALaunchedAgent(t *testing.T) {
	var errOut bytes.Buffer
	finish := startTrace("handoff", &errOut, withTrace(Env{}, "1"))
	finishTraceNow()
	written := errOut.String()
	if !strings.Contains(written, "agent-archive trace") {
		t.Fatalf("finishTraceNow wrote nothing:\n%s", written)
	}
	finish()
	if errOut.String() != written {
		t.Fatalf("the command's own finish wrote the trace again:\n%s", errOut.String())
	}
	for _, kv := range childEnv([]string{envTrace + "=1", "HOME=/h"}) {
		if strings.HasPrefix(kv, envTrace+"=") {
			t.Fatalf("a launched agent inherits %s", kv)
		}
	}
}
