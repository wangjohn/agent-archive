package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testHook(executable string) Hook { return Hook{Executable: executable} }

func TestMergePreservesAndIsIdempotent(t *testing.T) {
	for _, app := range []string{"codex", "claude", "cursor"} {
		t.Run(app, func(t *testing.T) {
			original := []byte(`{"unrelated":true,"hooks":{}}`)
			first, e := Merge(original, app, testHook("/Applications/Agent Archive/bin/agent-archive"))
			if e != nil {
				t.Fatal(e)
			}
			second, e := Merge(first, app, testHook("/Applications/Agent Archive/bin/agent-archive"))
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("duplicate hooks on setup rerun")
			}
			var value map[string]any
			if err := json.Unmarshal(first, &value); err != nil {
				t.Fatal(err)
			}
			if value["unrelated"] != true {
				t.Fatal("lost setting")
			}
			if bytes.Contains(first, []byte("PostToolUse")) {
				t.Fatal("per-tool hook installed")
			}
		})
	}
}

func TestPreserveUnrelatedHandler(t *testing.T) {
	data, e := Merge([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"}]}]}}`), "codex", testHook("/bin/agent-archive"))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(data, []byte(`"command":"other"`)) {
		t.Fatalf("removed unrelated handler:\n%s", data)
	}
}

func TestInvalidConfigIsNotOverwritten(t *testing.T) {
	for _, s := range []string{`null`, `[]`, `{"hooks":42}`, `{"hooks":{"Stop":[{}]}}`, `{"a":1} {"b":2}`, `{"a":1,}`} {
		if _, e := Merge([]byte(s), "codex", testHook("/bin/archive")); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestMigratePrototypeOnly(t *testing.T) {
	raw := []byte(`{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"python old.py hook","statusMessage":"Recording private skill-run evidence"},{"type":"command","command":"echo keep"}]}]}}`)
	result, e := Merge(raw, "codex", testHook("/tmp/agent-archive"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(result), "old.py") || !strings.Contains(string(result), "echo keep") {
		t.Fatal(string(result))
	}
}

// Setup edits files the user and their applications own. Everything outside
// the hooks member must keep its bytes (key order, escaping, number
// literals), the user's own handlers inside it must keep their key order and
// values, and uninstall must hand back the file exactly as it was.
func TestMergeAndRemoveKeepTheUsersFile(t *testing.T) {
	files := map[string]string{
		// Claude Code's own layout: two-space JSON, unsorted keys.
		"no hooks yet": `{
  "model": "opus",
  "env": {
    "BUILD": "test -f x && echo <done>",
    "Z": "1"
  },
  "big": 12345678901234567890,
  "precise": 1.10,
  "alwaysThinkingEnabled": true
}
`,
		"the user's own hooks": `{
  "permissions": {
    "allow": [
      "Bash(make test && make lint)"
    ]
  },
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "say 'done' && echo <ok>",
            "timeout": 12345678901234567890
          }
        ]
      }
    ],
    "Notification": []
  },
  "zeta": 1
}
`,
		"four-space indentation": "{\n    \"b\": 1,\n    \"a\": [1, 2]\n}",
		"compact":                `{"b":1,"a":"x<y"}`,
		"empty object":           "{}\n",
	}
	for name, original := range files {
		t.Run(name, func(t *testing.T) {
			merged, err := Merge([]byte(original), "claude", testHook("/usr/local/bin/agent-archive"))
			if err != nil {
				t.Fatal(err)
			}
			for _, kept := range []string{"&&", "<done>", "<ok>", "x<y", "12345678901234567890", "1.10"} {
				if strings.Contains(original, kept) && !strings.Contains(string(merged), kept) {
					t.Errorf("%q was rewritten:\n%s", kept, merged)
				}
			}
			if strings.Contains(string(merged), `\u00`) {
				t.Errorf("merge HTML-escaped the file:\n%s", merged)
			}
			if i := strings.Index(original, `"hooks"`); i < 0 && original != "{}\n" {
				// Everything before the closing brace is untouched.
				body := strings.TrimRight(original, "}\n ")
				if !strings.HasPrefix(string(merged), body) {
					t.Errorf("merge rewrote the rest of the file:\n%s", merged)
				}
			}
			if name == "the user's own hooks" {
				// The user's handler keeps its key order and comes first.
				if !regexp.MustCompile(`"type": "command",\s+"command": "say 'done' && echo <ok>",`).Match(merged) {
					t.Errorf("user handler reordered:\n%s", merged)
				}
				if !strings.Contains(string(merged), "\"Notification\": []") {
					t.Errorf("an empty event of the user's was dropped:\n%s", merged)
				}
			}
			var parsed any
			if err := json.Unmarshal(merged, &parsed); err != nil {
				t.Fatalf("merge produced invalid JSON: %v\n%s", err, merged)
			}
			removed, changed, err := Remove(merged, "claude")
			if err != nil || !changed {
				t.Fatalf("remove: changed=%v err=%v", changed, err)
			}
			if string(removed) != original {
				t.Fatalf("uninstall did not restore the file\nwant:\n%s\ngot:\n%s", original, removed)
			}
		})
	}
}

// The data directory reaches the hook even when the application runs it
// with none of the user's shell environment (Cursor started from the Dock),
// and quoting holds for paths with spaces and quotes.
func TestHookCommandCarriesTheDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "agent archive")
	out := filepath.Join(dir, "seen")
	script := "#!/bin/sh\nprintf '%s|%s' \"$AGENT_ARCHIVE_HOME\" \"$*\" > '" + strings.ReplaceAll(out, "'", `'"'"'`) + "'\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dataHome := filepath.Join(dir, "data $HOME `x`")
	command, err := Hook{Executable: executable, DataHome: dataHome}.Command("cursor")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	seen, _ := os.ReadFile(out)
	if want := dataHome + "|_hook --harness cursor"; string(seen) != want {
		t.Fatalf("hook saw %q, want %q", seen, want)
	}
	plain, _ := Hook{Executable: executable}.Command("cursor")
	if strings.Contains(plain, "AGENT_ARCHIVE_HOME") {
		t.Fatalf("default data directory embedded: %s", plain)
	}
	if _, err := (Hook{Executable: executable, DataHome: "relative"}).Command("claude"); err == nil {
		t.Fatal("accepted a relative data directory")
	}
}
