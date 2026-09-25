package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// H-22: files the user or an app wrote in a form that is not plain JSON are
// refused, and the refusal says where and, for the usual suspects, what.
func TestInvalidHookFilesSayWhereAndWhat(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"byte-order mark", "\xef\xbb\xbf{\"a\":1}", "byte-order mark"},
		{"line comment", "{\n  // mine\n  \"a\": 1\n}\n", "line 2, column 3: .*comments \\(JSONC\\)"},
		{"block comment", "{\n  \"a\": 1 /* x */\n}\n", "line 2, column 10: .*comments \\(JSONC\\)"},
		{"trailing comma in object", "{\n  \"a\": 1,\n}\n", "line 2, column 9: .*trailing comma"},
		{"trailing comma in array", "{\"a\": [1, 2,]}", "line 1, column 12: .*trailing comma"},
		{"truncated", "{\"a\": ", "line 1, column 7"},
		{"not an object", "[]", "one JSON object"},
		{"two values", "{} {}", "more than one JSON value"},
		{"CRLF, broken", "{\r\n  \"a\": ,\r\n}\r\n", "line 2, column 8"},
	}
	for _, c := range cases {
		for _, harness := range []string{"claude", "cursor"} {
			_, err := Merge([]byte(c.input), harness, testHook("/bin/agent-archive"))
			if !errors.Is(err, errInvalidConfiguration) || !regexp.MustCompile(c.want).MatchString(err.Error()) {
				t.Errorf("%s (%s): %v, want %q", c.name, harness, err, c.want)
			}
		}
	}
}

// Two same-named keys anywhere under "hooks" are refused, like two "hooks"
// keys: the apps keep the last, and editing one would drop the other's
// handlers without a word (H-22: a duplicate event key lost handler A).
func TestDuplicateKeysUnderHooksAreRefused(t *testing.T) {
	for _, input := range []string{
		`{"hooks": {"Stop": [{"hooks":[{"type":"command","command":"first"}]}], "Stop": [{"hooks":[{"type":"command","command":"second"}]}]}}`,
		`{"hooks": {"Stop": [{"hooks":[{"type":"command","command":"a"}], "hooks":[{"type":"command","command":"b"}]}]}}`,
		`{"hooks": {"Stop": [{"hooks":[{"type":"command","command":"a","command":"b"}]}]}}`,
	} {
		for _, run := range []func() error{
			func() error { _, err := Merge([]byte(input), "claude", testHook("/bin/agent-archive")); return err },
			func() error { _, _, err := Remove([]byte(input), "claude", Hook{}); return err },
		} {
			if err := run(); err == nil || !strings.Contains(err.Error(), "remove the duplicate") {
				t.Errorf("%s: %v", input, err)
			}
		}
	}
	// Elsewhere in the file a duplicate is the user's business, and is kept.
	input := `{"a": 1, "a": 2}`
	merged, err := Merge([]byte(input), "claude", testHook("/bin/agent-archive"))
	if err != nil || !strings.HasPrefix(string(merged), `{"a": 1, "a": 2`) {
		t.Fatalf("%v\n%s", err, merged)
	}
}

// A file whose first key shares the opening brace's line still gets new
// members on lines of their own, indented like its other members (H-22: it
// got a compact one-line block).
func TestMixedLayoutGetsIndentedMembers(t *testing.T) {
	input := "{\"a\": 1,\n    \"b\": 2\n}\n"
	merged, err := Merge([]byte(input), "claude", testHook("/bin/agent-archive"))
	must(t, err)
	if !strings.Contains(string(merged), "\n    \"hooks\": {\n        \"SessionStart\"") {
		t.Fatalf("not indented like the file:\n%s", merged)
	}
	removed, _, err := Remove(merged, "claude", Hook{})
	if err != nil || string(removed) != input {
		t.Fatalf("round trip: %v\n%q", err, removed)
	}
}

// A hook file setup created is deleted again by uninstall, Cursor's included
// (H-22: it was left as {"version": 1}). A file that is a link a dotfile
// manager keeps is emptied instead, and one with anything else in it keeps
// that.
func TestRemovalDeletesAFileSetupCreated(t *testing.T) {
	for _, app := range []string{"claude", "codex", "cursor"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			files := testFiles(home)
			changes, err := Plan(files, testHook("/bin/agent-archive"), []string{app})
			must(t, err)
			must(t, Apply(changes))
			removal, err := PlanRemoval(files, Hook{}, []string{app})
			must(t, err)
			if len(removal) != 1 || !removal[0].Delete {
				t.Fatalf("removal: %+v", removal)
			}
			must(t, Apply(removal))
			if _, err := os.Stat(files[app]); !os.IsNotExist(err) {
				t.Fatalf("still there: %v", err)
			}
			// Rolled back, it comes back as it was.
			must(t, Rollback(removal))
			if ok, err := Installed(files, testHook("/bin/agent-archive"), app); !ok || err != nil {
				t.Fatalf("rollback: %v %v", ok, err)
			}

			// Through a symlink: the link stays, holding an empty object.
			target := filepath.Join(t.TempDir(), "dotfiles-hooks.json")
			data, err := os.ReadFile(files[app])
			must(t, err)
			must(t, os.WriteFile(target, data, 0o600))
			must(t, os.Remove(files[app]))
			must(t, os.Symlink(target, files[app]))
			removal, err = PlanRemoval(files, Hook{}, []string{app})
			must(t, err)
			if len(removal) != 1 || removal[0].Delete {
				t.Fatalf("symlink removal: %+v", removal)
			}
			must(t, Apply(removal))
			if info, err := os.Lstat(files[app]); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the link was not kept: %v", err)
			}
			left, _ := os.ReadFile(target)
			if !Empty(left) {
				t.Fatalf("left %q", left)
			}
		})
	}
	// Cursor's version stays when anything else is in the file.
	merged, err := Merge([]byte(`{"x": 1}`), "cursor", testHook("/bin/agent-archive"))
	must(t, err)
	removed, _, err := Remove(merged, "cursor", Hook{})
	must(t, err)
	if !bytes.Contains(removed, []byte(`"version"`)) || !bytes.Contains(removed, []byte(`"x": 1`)) && !bytes.Contains(removed, []byte(`"x":1`)) {
		t.Fatalf("%s", removed)
	}
}

// A write that fails before its final rename left the file as it was, so
// Apply reports that one failure, not a second one from putting back a file
// that never changed (H-22).
func TestApplyReportsOneErrorWhenTheWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := t.TempDir()
	files := testFiles(home)
	dir := filepath.Dir(files["claude"])
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(files["claude"], []byte("{}\n"), 0o600))
	changes, err := Plan(files, testHook("/bin/agent-archive"), []string{"claude"})
	must(t, err)
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err = Apply(changes)
	if err == nil {
		t.Fatal("the write succeeded")
	}
	if n := strings.Count(err.Error(), "permission denied"); n != 1 {
		t.Fatalf("%d failures reported: %v", n, err)
	}
	must(t, os.Chmod(dir, 0o700))
	if data, _ := os.ReadFile(files["claude"]); string(data) != "{}\n" {
		t.Fatalf("file changed: %q", data)
	}
}

// FuzzMergeRemove checks, for any file Merge accepts: the result is JSON with
// the hooks installed, merging again changes nothing, and Remove takes out
// exactly what Merge put in: the user's own settings and handlers are
// unchanged, and a file without a "hooks" member (and, for Cursor, with its
// "version") gets its bytes back.
func FuzzMergeRemove(f *testing.F) {
	for _, seed := range []string{
		"", "{}", "{}\n", `{"model":"opus"}`, "{\n\t\"model\": \"opus\"\n}\n",
		"{\r\n  \"model\": \"opus\"\r\n}\r\n", "{\"a\": 1,\n  \"b\": 2\n}\n",
		"{\n  \"hooks\": {},\n  \"model\": \"x\"\n}\n", `{"hooks": null}`,
		`{"version":1,"hooks":{"stop":[{"command":"echo hi"}]}}`, `{"x":1}`,
		"{\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"echo hi\"}]}\n    ]\n  }\n}\n",
		`{"n": 12345678901234567890, "s": "a&b<c>é\/"}`, "\xef\xbb\xbf{}", `{"a":1,}`,
	} {
		for harness := range uint8(3) {
			f.Add([]byte(seed), harness)
		}
	}
	f.Fuzz(func(t *testing.T, input []byte, which uint8) {
		harness := []string{"claude", "codex", "cursor"}[which%3]
		if bytes.Contains(input, []byte(Owner)) || bytes.Contains(input, []byte(prototypeOwner)) {
			return
		}
		hook := testHook("/usr/local/bin/agent-archive")
		merged, err := Merge(input, harness, hook)
		if err != nil {
			return
		}
		if !json.Valid(merged) {
			t.Fatalf("invalid JSON:\n%s", merged)
		}
		again, err := Merge(merged, harness, hook)
		if err != nil || !bytes.Equal(again, merged) {
			t.Fatalf("merge is not idempotent (%v):\n%s\n---\n%s", err, merged, again)
		}
		removed, changed, err := Remove(merged, harness, Hook{})
		if err != nil || !changed {
			t.Fatalf("remove: changed=%v err=%v", changed, err)
		}
		if !json.Valid(removed) {
			t.Fatalf("remove left invalid JSON:\n%s", removed)
		}
		original, err := parseDocument(input)
		if err != nil {
			t.Fatalf("merge accepted what parse refuses: %v", err)
		}
		_, hadHooks := original.root.get("hooks")
		version, hadVersion := original.root.get("version")
		// An object with no members at all is laid out afresh when the hooks
		// go in, so only its emptiness comes back, not the whitespace inside
		// its braces.
		// Cursor's lone "version" goes with the hooks, since it is what
		// setup adds to a file it creates (see Remove).
		empty := len(original.root.members) == 0 || harness == "cursor" && !hadHooks && len(usersMembers(original.root).members) == 0
		if empty && !Empty(removed) {
			t.Fatalf("an empty file came back with %q", removed)
		}
		if !empty && !hadHooks && (harness != "cursor" || hadVersion && isOne(version)) && !bytes.Equal(removed, input) {
			t.Fatalf("round trip changed the file:\n%q\n---\n%q", input, removed)
		}
		// Whatever the shape, the user's members other than "hooks" and
		// "version" are untouched.
		left, err := parseDocument(removed)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(usersMembers(original.root), usersMembers(left.root)) {
			t.Fatalf("the user's settings changed:\n%q\n---\n%q", input, removed)
		}
	})
}

// usersMembers is root without the members setup owns, in file order.
func usersMembers(root *object) *object {
	out := &object{}
	for _, m := range root.members {
		//lint:ignore LV1001 top-level member names of a user's JSON file are an open set; these two are setup's
		if m.key != "hooks" && m.key != "version" {
			out.members = append(out.members, m)
		}
	}
	return out
}

func sameJSON(a, b any) bool {
	var x, y bytes.Buffer
	return encodeValue(&x, a) == nil && encodeValue(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
}
