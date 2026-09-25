package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A write that does not read back through its path (it landed somewhere
// else) fails Apply, and is taken back: the misplaced file and the
// directories created for it are removed, and earlier changes rolled back.
func TestApplyTakesBackAMisplacedWrite(t *testing.T) {
	home := t.TempDir()
	plan, err := Plan(testFiles(home), testHook("/usr/local/bin/agent-archive"), []string{"claude", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(home, "elsewhere", "deeper", "hooks.json")
	writeTarget := func(path string) (string, error) {
		if path == plan[1].Path {
			return stray, nil
		}
		return resolveTarget(path)
	}
	if err := apply(plan, writeTarget); err == nil || !strings.Contains(err.Error(), "does not read back") {
		t.Fatalf("Apply: %v", err)
	}
	for _, path := range []string{stray, filepath.Join(home, "elsewhere"), plan[0].Path, plan[1].Path} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s was left behind", path)
		}
	}
}

// A misplaced write over an existing file puts that file's content back.
func TestApplyRestoresAFileAMisplacedWriteReplaced(t *testing.T) {
	home := t.TempDir()
	plan, err := Plan(testFiles(home), testHook("/usr/local/bin/agent-archive"), []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "other.json")
	must(t, os.WriteFile(other, []byte("mine"), 0644))
	writeTarget := func(string) (string, error) { return other, nil }
	if err := apply(plan, writeTarget); err == nil {
		t.Fatal("a misplaced write succeeded")
	}
	if b, _ := os.ReadFile(other); string(b) != "mine" {
		t.Fatalf("the replaced file holds %q", b)
	}
}

func TestApplyRefusesToOverwriteATargetItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	home := t.TempDir()
	plan, err := Plan(testFiles(home), testHook("/usr/local/bin/agent-archive"), []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "other.json")
	must(t, os.WriteFile(other, []byte("mine"), 0200))
	t.Cleanup(func() { _ = os.Chmod(other, 0600) })
	writeTarget := func(string) (string, error) { return other, nil }
	if err := apply(plan, writeTarget); err == nil {
		t.Fatal("a write over an unreadable file succeeded")
	}
	must(t, os.Chmod(other, 0600))
	if b, _ := os.ReadFile(other); string(b) != "mine" {
		t.Fatalf("the unreadable file now holds %q", b)
	}
}
