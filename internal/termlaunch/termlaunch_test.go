package termlaunch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// hostile are words a shell, AppleScript, or tmux would misread if they were
// ever interpolated unquoted.
var hostile = []string{
	"it's",
	`'\''`,
	"$(touch pwned)",
	"`touch pwned`",
	"${HOME}",
	"a\nb\n",
	"  two  spaces  ",
	"ünïcødé ✓",
	`back\slash`,
	`"double"`,
	"",
	"-n",
	"#{session_name} #(touch pwned)",
	"*",
	"; rm -rf ~ &",
}

type call struct {
	name string
	args []string
}

// fakeEnv records every command and fails those failing names.
func fakeEnv(goos string, vars map[string]string, failing string) (Environment, *[]call) {
	var calls []call
	return Environment{
		GOOS: goos,
		LookupEnv: func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		},
		Run: func(_ context.Context, name string, args ...string) error {
			calls = append(calls, call{name, args})
			if failing != "" && slices.Contains(args, `tell application "`+failing+`"`) {
				return errors.New("osascript: execution error")
			}
			return nil
		},
	}, &calls
}

func testSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		Dir:       "/work/app #1",
		Argv:      []string{"codex", "--cd", "/work/app #1", "read it's $(here)"},
		Unset:     []string{"CODEX_THREAD_ID"},
		ScriptDir: t.TempDir(),
	}
}

var scriptName = regexp.MustCompile(`^launch-[0-9a-f]{24}\.sh$`)

// scriptPath returns the one launcher script in dir.
func scriptPath(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !scriptName.MatchString(entries[0].Name()) {
		t.Fatalf("script directory holds %v, want one launch-<hex>.sh", entries)
	}
	return filepath.Join(dir, entries[0].Name())
}

func osascript(lines []string, argv ...string) call {
	return call{"osascript", osascriptArgs(lines, argv...)}
}

func TestOpenPicksTheTerminal(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		vars    map[string]string
		failing string
		where   string
		calls   func(script, dir string) []call
	}{
		{
			name:  "tmux",
			goos:  "linux",
			vars:  map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM_PROGRAM": "iTerm.app"},
			where: "a new tmux window",
			calls: func(script, _ string) []call {
				return []call{{"tmux", []string{"new-window", "-c", "/work/app ##1", "'" + script + "'"}}}
			},
		},
		{
			name:  "iTerm2",
			goos:  "darwin",
			vars:  map[string]string{"TERM_PROGRAM": "iTerm.app"},
			where: "a new iTerm2 tab",
			calls: func(script, _ string) []call { return []call{osascript(iTermScript, script)} },
		},
		{
			name:  "Ghostty",
			goos:  "darwin",
			vars:  map[string]string{"TERM_PROGRAM": "ghostty"},
			where: "a new Ghostty tab",
			calls: func(script, dir string) []call { return []call{osascript(ghosttyScript, script, dir)} },
		},
		{
			name:    "Ghostty before 1.3 falls back to Terminal.app",
			goos:    "darwin",
			vars:    map[string]string{"TERM_PROGRAM": "ghostty"},
			failing: "Ghostty",
			where:   "a new Terminal window",
			calls: func(script, dir string) []call {
				return []call{osascript(ghosttyScript, script, dir), osascript(terminalScript, script)}
			},
		},
		{
			name:  "Terminal.app",
			goos:  "darwin",
			vars:  map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			where: "a new Terminal window",
			calls: func(script, _ string) []call { return []call{osascript(terminalScript, script)} },
		},
		{
			name:  "an unknown terminal on macOS",
			goos:  "darwin",
			vars:  map[string]string{"TERM_PROGRAM": "vscode", "TMUX": ""},
			where: "a new Terminal window",
			calls: func(script, _ string) []call { return []call{osascript(terminalScript, script)} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec(t)
			env, calls := fakeEnv(tt.goos, tt.vars, tt.failing)
			where, err := Open(context.Background(), spec, env)
			if err != nil {
				t.Fatal(err)
			}
			if where != tt.where {
				t.Errorf("where = %q, want %q", where, tt.where)
			}
			script := scriptPath(t, spec.ScriptDir)
			if want := tt.calls(script, spec.Dir); !reflect.DeepEqual(*calls, want) {
				t.Errorf("commands =\n%q\nwant\n%q", *calls, want)
			}
		})
	}
}

// The AppleScript is fixed text: values reach it only as run-handler
// arguments, never spliced into its source.
func TestAppleScriptSource(t *testing.T) {
	var b strings.Builder
	for _, lines := range [][]string{iTermScript, ghosttyScript, terminalScript} {
		b.WriteString(strings.Join(lines, "\n") + "\n\n")
	}
	golden.Check(t, "testdata/applescript.golden", []byte(b.String()))
}

func TestOpenRemovesTheScriptWhenTheTerminalFails(t *testing.T) {
	spec := testSpec(t)
	env, _ := fakeEnv("darwin", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, "Terminal")
	if _, err := Open(context.Background(), spec, env); err == nil {
		t.Fatal("Open succeeded, want the osascript error")
	}
	if entries, _ := os.ReadDir(spec.ScriptDir); len(entries) != 0 {
		t.Fatalf("script directory still holds %v", entries)
	}
}

func TestOpenWithoutATerminal(t *testing.T) {
	spec := testSpec(t)
	env, calls := fakeEnv("linux", map[string]string{"TERM_PROGRAM": "iTerm.app"}, "")
	_, err := Open(context.Background(), spec, env)
	if !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("err = %v, want ErrNoTerminal", err)
	}
	want := `cd '/work/app #1' && env -u CODEX_THREAD_ID 'codex' '--cd' '/work/app #1' 'read it'\''s $(here)'`
	if !strings.HasSuffix(err.Error(), "\n  "+want) {
		t.Errorf("err = %q, want it to end with the command %q", err, want)
	}
	if len(*calls) != 0 {
		t.Errorf("ran %q", *calls)
	}
	if entries, _ := os.ReadDir(spec.ScriptDir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestOpenRejectsUnsafeSpecs(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]Spec{
		"no command":            {Dir: "/work", ScriptDir: dir},
		"relative directory":    {Dir: "work", Argv: []string{"codex"}, ScriptDir: dir},
		"relative script dir":   {Dir: "/work", Argv: []string{"codex"}, ScriptDir: "scripts"},
		"NUL in an argument":    {Dir: "/work", Argv: []string{"codex", "a\x00b"}, ScriptDir: dir},
		"NUL in the directory":  {Dir: "/work\x00", Argv: []string{"codex"}, ScriptDir: dir},
		"bad variable name":     {Dir: "/work", Argv: []string{"codex"}, Unset: []string{"A; rm -rf ~"}, ScriptDir: dir},
		"variable with a digit": {Dir: "/work", Argv: []string{"codex"}, Unset: []string{"1A"}, ScriptDir: dir},
		"empty variable name":   {Dir: "/work", Argv: []string{"codex"}, Unset: []string{""}, ScriptDir: dir},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			env, calls := fakeEnv("darwin", map[string]string{"TMUX": "x"}, "")
			if _, err := Open(context.Background(), spec, env); err == nil {
				t.Fatal("Open accepted the spec")
			}
			if len(*calls) != 0 {
				t.Errorf("ran %q", *calls)
			}
		})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestScriptIsPrivateAndExecutable(t *testing.T) {
	spec := testSpec(t)
	path, err := writeScript(spec)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %v, want 0700", got)
	}
	if filepath.Dir(path) != spec.ScriptDir || !scriptName.MatchString(filepath.Base(path)) {
		t.Errorf("path = %s", path)
	}
}

func TestScriptText(t *testing.T) {
	spec := Spec{
		Dir:   "/work/it's $(here) `x` ü\nnext",
		Argv:  append([]string{"/usr/local/bin/agent"}, hostile...),
		Unset: []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"},
	}
	golden.Check(t, "testdata/hostile.sh.golden", []byte(script(spec, "/private/scripts/launch-0123.sh")))
}

// runScript writes spec's launcher and runs it with /bin/sh as a terminal
// would, with SECRET set in its environment.
func runScript(t *testing.T, spec Spec, stdin string) (output string, code int) {
	t.Helper()
	path, err := writeScript(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), path)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "SECRET=leaked")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the script did not remove itself: %v", err)
	}
	return string(out), code
}

func TestScriptPassesArgumentsByteForByte(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "it's $(here) `x` ü\nnext #1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	// The child writes each argument NUL-terminated, then its directory and
	// whether SECRET survived.
	record := `out=$1; shift; for a; do printf '%s\0' "$a"; done > "$out"; pwd > "$out.dir"; printf %s "${SECRET-unset}" > "$out.env"`
	spec := Spec{
		Dir:       dir,
		Argv:      append([]string{"/bin/sh", "-c", record, "sh", out}, hostile...),
		Unset:     []string{"SECRET"},
		ScriptDir: t.TempDir(),
	}
	if output, code := runScript(t, spec, ""); code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(hostile, "\x00") + "\x00"; string(got) != want {
		t.Errorf("arguments = %q, want %q", got, want)
	}
	if pwd, _ := os.ReadFile(out + ".dir"); string(pwd) != dir+"\n" {
		t.Errorf("directory = %q, want %q", pwd, dir)
	}
	if env, _ := os.ReadFile(out + ".env"); string(env) != "unset" {
		t.Errorf("SECRET = %q, want it unset", env)
	}
	for _, d := range []string{root, dir, spec.ScriptDir} {
		if _, err := os.Stat(filepath.Join(d, "pwned")); err == nil {
			t.Errorf("a quoted word ran a command in %s", d)
		}
	}
}

func TestScriptWaitsAfterAFailure(t *testing.T) {
	spec := Spec{Dir: t.TempDir(), Argv: []string{"/bin/sh", "-c", "exit 3"}, ScriptDir: t.TempDir()}
	output, code := runScript(t, spec, "\n")
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	if !strings.Contains(output, "/bin/sh exited with status 3. Press Enter to close.") {
		t.Errorf("output = %q", output)
	}
}

func TestScriptWaitsWhenTheDirectoryIsGone(t *testing.T) {
	spec := Spec{Dir: filepath.Join(t.TempDir(), "gone"), Argv: []string{"/usr/bin/true"}, ScriptDir: t.TempDir()}
	output, code := runScript(t, spec, "\n")
	if code != 1 || !strings.Contains(output, "Press Enter to close.") {
		t.Errorf("exit %d, output %q", code, output)
	}
}

// FuzzShellQuote checks that /bin/sh reads a quoted word back unchanged.
func FuzzShellQuote(f *testing.F) {
	for _, s := range hostile {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.IndexByte(s, 0) >= 0 {
			t.Skip("NUL cannot be an argument")
		}
		out, err := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		if !bytes.Equal(out, []byte(s)) {
			t.Fatalf("round trip = %q, want %q", out, s)
		}
	})
}
