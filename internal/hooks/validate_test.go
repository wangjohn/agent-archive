package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Validate finds every file Plan refuses, before setup asks anything, and
// says where and what: the same checks, and word for word the same error.
func TestValidateReportsWhereAndWhatPlanRefuses(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		input   string
		line    int
		column  int
		reason  string
	}{
		{"line comment", "claude", "{\n  // mine\n  \"model\": \"x\"\n}\n", 2, 3, "comments (JSONC) are not JSON"},
		{"block comment", "cursor", "{\n  \"version\": 1 /* x */\n}\n", 2, 16, "comments (JSONC) are not JSON"},
		{"trailing comma in object", "codex", "{\n  \"hooks\": {},\n}\n", 2, 14, "trailing comma"},
		{"trailing comma in array", "claude", `{"a": [1, 2,]}`, 1, 12, "trailing comma"},
		{"comment after the object", "claude", "{\"a\": 1}\n// mine\n", 2, 1, "comments (JSONC) are not JSON"},
		{"comma after the object", "codex", "{\"a\": 1},\n", 1, 9, "trailing comma"},
		{"byte-order mark", "claude", "\xef\xbb\xbf{\"a\":1}", 1, 1, "byte-order mark (BOM)"},
		{"CRLF, broken", "claude", "{\r\n  \"a\": ,\r\n}\r\n", 2, 8, "invalid character ','"},
		{"truncated", "codex", `{"a": `, 1, 7, "EOF"},
		{"not an object", "claude", "\n  []", 2, 3, "the file must hold one JSON object"},
		{"two values", "claude", "{}\n{}", 2, 1, "more than one JSON value"},
		{"two top-level hooks keys", "claude", "{\n  \"hooks\": {},\n  \"hooks\": {}\n}", 3, 3, `more than one top-level "hooks" key`},
		{"two top-level version keys", "cursor", `{"version": 1, "version": 1}`, 1, 16, `more than one top-level "version" key`},
		{"duplicate event under hooks", "claude", `{"hooks": {"Stop": [], "Stop": []}}`, 0, 0, `"hooks" has more than one "Stop" key; remove the duplicate`},
		{"duplicate key in a handler", "claude", `{"hooks": {"Stop": [{"hooks": [{"command": "a", "command": "b"}]}]}}`, 0, 0, `"hooks"."Stop"[0]."hooks"[0] has more than one "command" key`},
		{"hooks not an object", "claude", `{"hooks": []}`, 0, 0, "invalid hooks object"},
		{"event not a list", "codex", `{"hooks": {"Stop": {}}}`, 0, 0, "invalid hook list for Stop"},
		{"handlers not a list", "claude", `{"hooks": {"Stop": [{"hooks": {}}]}}`, 0, 0, "invalid hook handlers"},
		{"Cursor version 2", "cursor", `{"version": 2, "hooks": {}}`, 0, 0, "unsupported Cursor hook configuration version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			files := testFiles(home)
			path := files[c.harness]
			must(t, os.MkdirAll(filepath.Dir(path), 0700))
			must(t, os.WriteFile(path, []byte(c.input), 0600))

			problems := Validate(files)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want one", problems)
			}
			p := problems[0]
			if p.Harness != c.harness || p.Path != path || p.Line != c.line || p.Column != c.column || !strings.Contains(p.Reason, c.reason) {
				t.Errorf("got %s %s line %d column %d %q, want %s %s line %d column %d %q", p.Harness, p.Path, p.Line, p.Column, p.Reason, c.harness, path, c.line, c.column, c.reason)
			}
			if strings.Contains(p.Reason, errInvalidConfiguration.Error()) || strings.Contains(p.Reason, path) {
				t.Errorf("reason %q repeats what the other fields say", p.Reason)
			}
			_, planErr := Plan(files, testHook("/Applications/agent-archive"), []string{c.harness})
			if planErr == nil || p.Err == nil || p.Err.Error() != planErr.Error() {
				t.Errorf("Err = %v, Plan fails with %v", p.Err, planErr)
			}
		})
	}
}

// Files Plan accepts are no problem, including missing and empty ones and
// one setup already installed into.
func TestValidateAcceptsWhatPlanAccepts(t *testing.T) {
	home := t.TempDir()
	files := testFiles(home)
	must(t, os.MkdirAll(filepath.Dir(files["cursor"]), 0700))
	must(t, os.WriteFile(files["cursor"], []byte("  \n"), 0600))
	must(t, os.MkdirAll(filepath.Dir(files["claude"]), 0700))
	must(t, os.WriteFile(files["claude"], []byte(`{"permissions": {"allow": ["Read"]}, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}]}]}}`), 0600))
	if problems := Validate(files); len(problems) != 0 {
		t.Fatalf("before setup: %+v", problems)
	}
	plan, err := Plan(files, testHook("/Applications/agent-archive"), []string{"claude", "codex", "cursor"})
	must(t, err)
	must(t, Apply(plan))
	if problems := Validate(files); len(problems) != 0 {
		t.Fatalf("after setup: %+v", problems)
	}
}

// Validate reads the file a symbolic link names, as the applications and
// Plan do, but reports it by the path setup would edit.
func TestValidateFollowsSymlinks(t *testing.T) {
	home := t.TempDir()
	files := testFiles(home)
	dotfiles := filepath.Join(home, "dotfiles")
	must(t, os.MkdirAll(filepath.Join(dotfiles, "claude"), 0700))
	must(t, os.MkdirAll(filepath.Join(dotfiles, "cursor"), 0700))
	// A linked file holding a comment, inside a linked directory.
	must(t, os.WriteFile(filepath.Join(dotfiles, "claude", "real.json"), []byte("{\n  // mine\n}\n"), 0600))
	must(t, os.Symlink(filepath.Join(dotfiles, "claude"), filepath.Join(home, ".claude")))
	must(t, os.Symlink("real.json", files["claude"]))
	// A linked file that is fine.
	must(t, os.WriteFile(filepath.Join(dotfiles, "cursor", "hooks.json"), []byte(`{"version": 1}`), 0600))
	must(t, os.MkdirAll(filepath.Dir(files["cursor"]), 0700))
	must(t, os.Symlink(filepath.Join(dotfiles, "cursor", "hooks.json"), files["cursor"]))
	// A link to nothing yet, which setup creates like a missing file.
	must(t, os.MkdirAll(filepath.Dir(files["codex"]), 0700))
	must(t, os.Symlink(filepath.Join(dotfiles, "codex", "hooks.json"), files["codex"]))

	problems := Validate(files)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one for claude", problems)
	}
	p := problems[0]
	if p.Harness != "claude" || p.Path != files["claude"] || p.Line != 2 || p.Column != 3 || !strings.Contains(p.Reason, "JSONC") {
		t.Errorf("got %+v", p)
	}
	_, planErr := Plan(files, testHook("/Applications/agent-archive"), []string{"claude"})
	if planErr == nil || p.Err.Error() != planErr.Error() {
		t.Errorf("Err = %v, Plan fails with %v", p.Err, planErr)
	}
}

// Validate only reads: every file, link, mode, and time stamp is as it was,
// and nothing is created, not even the directory of a missing file.
func TestValidateChangesNothing(t *testing.T) {
	home := t.TempDir()
	files := testFiles(home)
	must(t, os.MkdirAll(filepath.Dir(files["claude"]), 0755))
	must(t, os.WriteFile(files["claude"], []byte("{\n  \"hooks\": {},\n}\n"), 0644))
	must(t, os.MkdirAll(filepath.Join(home, "dotfiles"), 0700))
	must(t, os.WriteFile(filepath.Join(home, "dotfiles", "cursor.json"), []byte(`{"version": 1, "hooks": {"stop": [{"command": "mine"}]}}`), 0600))
	must(t, os.MkdirAll(filepath.Dir(files["cursor"]), 0700))
	must(t, os.Symlink(filepath.Join(home, "dotfiles", "cursor.json"), files["cursor"]))
	// Time stamps in the past, so a rewrite with the same bytes would show.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	must(t, os.Chtimes(files["claude"], past, past))
	must(t, os.Chtimes(filepath.Join(home, "dotfiles", "cursor.json"), past, past))

	before := tree(t, home)
	if problems := Validate(files); len(problems) != 1 || problems[0].Harness != "claude" {
		t.Fatalf("problems = %+v, want one for claude", problems)
	}
	if after := tree(t, home); after != before {
		t.Errorf("Validate changed the files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A file that is there but cannot be read, like a directory in its place,
// is a problem, reported as Plan reports it.
func TestValidateReportsUnreadableFiles(t *testing.T) {
	home := t.TempDir()
	files := testFiles(home)
	must(t, os.MkdirAll(files["codex"], 0700))
	problems := Validate(files)
	if len(problems) != 1 || problems[0].Harness != "codex" || problems[0].Line != 0 || !strings.HasPrefix(problems[0].Reason, "the file cannot be read") {
		t.Fatalf("problems = %+v", problems)
	}
	_, planErr := Plan(files, testHook("/Applications/agent-archive"), []string{"codex"})
	if planErr == nil || problems[0].Err.Error() != planErr.Error() {
		t.Errorf("Err = %v, Plan fails with %v", problems[0].Err, planErr)
	}
}

// Every problem is found, not just the first, and they come in harness
// order.
func TestValidateReportsEveryFile(t *testing.T) {
	home := t.TempDir()
	files := testFiles(home)
	for _, harness := range []string{"cursor", "codex", "claude"} {
		must(t, os.MkdirAll(filepath.Dir(files[harness]), 0700))
		must(t, os.WriteFile(files[harness], []byte("{,}"), 0600))
	}
	files["gemini"] = filepath.Join(home, ".gemini", "settings.json")
	var got []string
	for _, p := range Validate(files) {
		got = append(got, p.Harness)
	}
	if strings.Join(got, " ") != "claude codex cursor gemini" {
		t.Errorf("problems for %v", got)
	}
}

// The structured parts of an invalid file's error do not change what setup
// shows or how callers match it.
func TestInvalidConfigurationErrorsStillWrapTheirCause(t *testing.T) {
	_, err := Merge([]byte("{\n  \"a\": x\n}"), "claude", testHook("/bin/agent-archive"))
	var syntaxErr *json.SyntaxError
	if !errors.Is(err, errInvalidConfiguration) || !errors.As(err, &syntaxErr) {
		t.Errorf("%v does not wrap both errInvalidConfiguration and the syntax error", err)
	}
	if want := "invalid existing hook configuration: line 2, column 8: invalid character 'x' looking for beginning of value"; err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
	_, err = Merge([]byte("\xef\xbb\xbf{}"), "claude", testHook("/bin/agent-archive"))
	if want := "invalid existing hook configuration: the file starts with a byte-order mark (BOM), which JSON does not allow; save it as UTF-8 without one"; err == nil || err.Error() != want {
		t.Errorf("message %q, want %q", err, want)
	}
}

// tree describes every entry under root: its path, type, mode, time stamp,
// and content or link target.
func tree(t *testing.T, root string) string {
	t.Helper()
	var out bytes.Buffer
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out.WriteString(rel + " " + info.Mode().String() + " " + info.ModTime().String())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out.WriteString(" -> " + link)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out.WriteString(" " + string(data))
		}
		out.WriteString("\n")
		return nil
	})
	must(t, err)
	return out.String()
}
