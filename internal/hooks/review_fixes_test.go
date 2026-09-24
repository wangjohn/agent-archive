package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative link is resolved from the directory it really sits in. With
// ~/.claude itself a link into a dotfiles repository, settings.json ->
// ../shared/settings.json names dotfiles/shared/settings.json, not
// ~/shared/settings.json.
func TestRelativeLinkInsideALinkedDirectory(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles")
	must(t, os.MkdirAll(filepath.Join(dotfiles, "claude"), 0700))
	must(t, os.MkdirAll(filepath.Join(dotfiles, "shared"), 0700))
	target := filepath.Join(dotfiles, "shared", "settings.json")
	original := []byte("{\n  \"model\": \"opus\"\n}\n")
	must(t, os.WriteFile(target, original, 0600))
	if err := os.Symlink("../shared/settings.json", filepath.Join(dotfiles, "claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "claude"), filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	hook := testHook("/usr/local/bin/agent-archive")
	plan, err := Plan(testFiles(home), hook, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "shared")); !os.IsNotExist(err) {
		t.Fatal("a stray file was written beside the linked directory")
	}
	if ok, err := Installed(testFiles(home), hook, "claude"); !ok || err != nil {
		t.Fatalf("installed=%v err=%v", ok, err)
	}
	if err := Rollback(plan); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != string(original) {
		t.Fatalf("rollback did not restore the target:\n%s", b)
	}
}

func TestDuplicateHooksKeyIsRefused(t *testing.T) {
	for _, src := range []string{`{"hooks":{},"model":"x","hooks":{}}`, `{"version":1,"version":1,"hooks":{}}`} {
		_, err := Merge([]byte(src), "cursor", testHook("/bin/agent-archive"))
		if !errors.Is(err, errInvalidConfiguration) || !strings.Contains(err.Error(), "more than one") {
			t.Fatalf("%s: %v", src, err)
		}
	}
}

// A file with Windows line endings keeps them in the spliced block, and
// uninstall restores it byte for byte.
func TestCRLFFileKeepsItsLineEndings(t *testing.T) {
	original := "{\r\n  \"model\": \"opus\",\r\n  \"hooks\": {\r\n    \"Notification\": []\r\n  }\r\n}\r\n"
	merged, err := Merge([]byte(original), "claude", testHook("/usr/local/bin/agent-archive"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(merged), "\n") != strings.Count(string(merged), "\r\n") {
		t.Fatalf("bare LF in a CRLF file:\n%q", merged)
	}
	removed, _, err := Remove(merged, "claude")
	if err != nil || string(removed) != original {
		t.Fatalf("round trip: %v\n%q", err, removed)
	}
}

// Cursor's "version" goes before "hooks", as Cursor writes it.
func TestCursorVersionPrecedesHooks(t *testing.T) {
	for name, src := range map[string]string{
		"new file":          "",
		"hooks, no version": "{\n  \"hooks\": {}\n}\n",
		"other settings":    "{\n  \"x\": 1\n}\n",
	} {
		merged, err := Merge([]byte(src), "cursor", testHook("/usr/local/bin/agent-archive"))
		if err != nil {
			t.Fatal(err)
		}
		v, h := strings.Index(string(merged), `"version"`), strings.Index(string(merged), `"hooks"`)
		if v < 0 || v > h {
			t.Fatalf("%s:\n%s", name, merged)
		}
		if ok, err := isValidJSON(merged); !ok {
			t.Fatalf("%s: %v\n%s", name, err, merged)
		}
	}
}

func isValidJSON(b []byte) (bool, error) {
	_, err := parseDocument(b)
	return err == nil, err
}
