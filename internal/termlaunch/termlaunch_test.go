package termlaunch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
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
func fakeEnv(system platform.OS, vars map[string]string, failing string) (Environment, *[]call) {
	var calls []call
	return Environment{
		OS: system,
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
		Argv:      []string{"/opt/bin/codex", "--cd", "/work/app #1", "read it's $(here)"},
		Unset:     []string{"CODEX_THREAD_ID"},
		ScriptDir: scriptDir(t),
	}
}

// scriptDir returns an empty directory only its owner can write to.
// t.TempDir creates its directory with the process's umask, so under umask
// 002 (a container's default user, for one) it is group-writable, and
// writeScript rightly refuses it.
func scriptDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
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
		system  platform.OS
		vars    map[string]string
		failing string
		where   string
		calls   func(script, dir string) []call
	}{
		{
			name:   "tmux",
			system: platform.Linux,
			vars:   map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM_PROGRAM": "iTerm.app"},
			where:  "a new tmux window",
			calls: func(script, _ string) []call {
				return []call{{"tmux", []string{"new-window", "-c", "/work/app ##1", "'" + script + "'"}}}
			},
		},
		{
			name:   "iTerm2",
			system: platform.Darwin,
			vars:   map[string]string{"TERM_PROGRAM": "iTerm.app"},
			where:  "a new iTerm2 tab",
			calls:  func(script, _ string) []call { return []call{osascript(iTermScript, script)} },
		},
		{
			name:   "Ghostty",
			system: platform.Darwin,
			vars:   map[string]string{"TERM_PROGRAM": "ghostty"},
			where:  "a new Ghostty tab",
			calls:  func(script, dir string) []call { return []call{osascript(ghosttyScript, script, dir)} },
		},
		{
			name:    "Ghostty before 1.3 falls back to Terminal.app",
			system:  platform.Darwin,
			vars:    map[string]string{"TERM_PROGRAM": "ghostty"},
			failing: "Ghostty",
			where:   "a new Terminal window",
			calls: func(script, dir string) []call {
				return []call{osascript(ghosttyScript, script, dir), osascript(terminalScript, script)}
			},
		},
		{
			name:   "Terminal.app",
			system: platform.Darwin,
			vars:   map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			where:  "a new Terminal window",
			calls:  func(script, _ string) []call { return []call{osascript(terminalScript, script)} },
		},
		{
			name:   "an unknown terminal on macOS",
			system: platform.Darwin,
			vars:   map[string]string{"TERM_PROGRAM": "vscode", "TMUX": ""},
			where:  "a new Terminal window",
			calls:  func(script, _ string) []call { return []call{osascript(terminalScript, script)} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec(t)
			env, calls := fakeEnv(tt.system, tt.vars, tt.failing)
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
	env, _ := fakeEnv(platform.Darwin, map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, "Terminal")
	if _, err := Open(context.Background(), spec, env); err == nil {
		t.Fatal("Open succeeded, want the osascript error")
	}
	if entries, _ := os.ReadDir(spec.ScriptDir); len(entries) != 0 {
		t.Fatalf("script directory still holds %v", entries)
	}
}

// Outside tmux, only macOS has a terminal Open drives: Linux, and a system
// the program does not know, are never treated as a Mac.
func TestOpenWithoutATerminal(t *testing.T) {
	// Values that are not any system the program knows.
	const (
		otherSystem platform.OS = "freebsd"
		noSystem    platform.OS = ""
	)
	for _, system := range []platform.OS{platform.Linux, platform.Unknown, otherSystem, noSystem} {
		spec := testSpec(t)
		env, calls := fakeEnv(system, map[string]string{"TERM_PROGRAM": "iTerm.app"}, "")
		_, err := Open(context.Background(), spec, env)
		if !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("%q: err = %v, want ErrNoTerminal", system, err)
		}
		want := `cd '/work/app #1' && env -u CODEX_THREAD_ID '/opt/bin/codex' '--cd' '/work/app #1' 'read it'\''s $(here)'`
		if !strings.HasSuffix(err.Error(), "\n  "+want) {
			t.Errorf("%q: err = %q, want it to end with the command %q", system, err, want)
		}
		if len(*calls) != 0 {
			t.Errorf("%q: ran %q", system, *calls)
		}
		if entries, _ := os.ReadDir(spec.ScriptDir); len(entries) != 0 {
			t.Errorf("%q: wrote %v", system, entries)
		}
	}
}

// The real environment answers for the system the process runs on.
func TestDefaultEnvironmentIsTheRunningSystem(t *testing.T) {
	if got := DefaultEnvironment().OS; got != platform.Current() {
		t.Errorf("DefaultEnvironment().OS = %q, want %q", got, platform.Current())
	}
}

func TestOpenRejectsUnsafeSpecs(t *testing.T) {
	dir := scriptDir(t)
	tests := map[string]Spec{
		"no command":            {Dir: "/work", ScriptDir: dir},
		"command found on PATH": {Dir: "/work", Argv: []string{"codex"}, ScriptDir: dir},
		"relative directory":    {Dir: "work", Argv: []string{"/opt/bin/codex"}, ScriptDir: dir},
		"relative script dir":   {Dir: "/work", Argv: []string{"/opt/bin/codex"}, ScriptDir: "scripts"},
		"NUL in an argument":    {Dir: "/work", Argv: []string{"/opt/bin/codex", "a\x00b"}, ScriptDir: dir},
		"NUL in the directory":  {Dir: "/work\x00", Argv: []string{"/opt/bin/codex"}, ScriptDir: dir},
		"bad variable name":     {Dir: "/work", Argv: []string{"/opt/bin/codex"}, Unset: []string{"A; rm -rf ~"}, ScriptDir: dir},
		"variable with a digit": {Dir: "/work", Argv: []string{"/opt/bin/codex"}, Unset: []string{"1A"}, ScriptDir: dir},
		"empty variable name":   {Dir: "/work", Argv: []string{"/opt/bin/codex"}, Unset: []string{""}, ScriptDir: dir},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			env, calls := fakeEnv(platform.Darwin, map[string]string{"TMUX": "x"}, "")
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

func TestOpenRefusesAScriptDirectoryOthersCanWrite(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"shared": shared, "missing": filepath.Join(root, "missing")} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t)
			spec.ScriptDir = dir
			env, calls := fakeEnv(platform.Darwin, map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, "")
			if _, err := Open(context.Background(), spec, env); err == nil {
				t.Fatal("Open accepted the script directory")
			}
			if len(*calls) != 0 {
				t.Errorf("ran %q", *calls)
			}
		})
	}
	if entries, _ := os.ReadDir(shared); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestRunReportsTheCommandsOutput(t *testing.T) {
	if err := run(context.Background(), "/bin/sh", "-c", "exit 0"); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), "/bin/sh", "-c", "echo 'no such window' >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "/bin/sh: exit status 2: no such window") {
		t.Errorf("err = %v", err)
	}
}

// The launcher is 0700 whatever the process's umask: a strict one must not
// strip the execute bit the terminal needs, and a loose one must not open
// the script to others.
//
// Regression: the tests used t.TempDir as the script directory, which is
// group-writable under umask 002, so writeScript refused it.
func TestScriptIsPrivateAndExecutableUnderAnyUmask(t *testing.T) {
	for _, umask := range []int{0o000, 0o002, 0o022, 0o077} {
		t.Run(fmt.Sprintf("umask %03o", umask), func(t *testing.T) {
			old := syscall.Umask(umask)
			t.Cleanup(func() { syscall.Umask(old) })
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
		})
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
	name := "it's $(here) `x` ü\nnext #1"
	dir := filepath.Join(root, name)
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
		ScriptDir: scriptDir(t),
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
	spec := Spec{Dir: t.TempDir(), Argv: []string{"/bin/sh", "-c", "exit 3"}, ScriptDir: scriptDir(t)}
	output, code := runScript(t, spec, "\n")
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	if !strings.Contains(output, "/bin/sh exited with status 3. Press Enter to close.") {
		t.Errorf("output = %q", output)
	}
}

func TestScriptWaitsWhenTheDirectoryIsGone(t *testing.T) {
	spec := Spec{Dir: filepath.Join(t.TempDir(), "gone"), Argv: []string{"/usr/bin/true"}, ScriptDir: scriptDir(t)}
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
