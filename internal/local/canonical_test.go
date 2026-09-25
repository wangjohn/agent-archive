package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// caseInsensitive reports whether dir's volume ignores case, as macOS's
// default APFS does; the case tests need one.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestCanonicalPathSpellsEachLocationOnce(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "Data", "Agent-Archive")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "Data"), link); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		target,
		target + "/",
		filepath.Join(dir, "Data", ".", "Agent-Archive"),
		filepath.Join(dir, "Data", "x", "..", "Agent-Archive"),
		filepath.Join(link, "Agent-Archive"),
	} {
		if got := CanonicalPath(spelling); got != target {
			t.Errorf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
		}
		if !SameLocation(spelling, target) {
			t.Errorf("SameLocation(%q, %q) = false", spelling, target)
		}
	}
	// What does not exist yet is kept as written, under the existing
	// part's canonical spelling.
	if got, want := CanonicalPath(filepath.Join(link, "Agent-Archive", "New", "Dir")), filepath.Join(target, "New", "Dir"); got != want {
		t.Errorf("CanonicalPath of a missing tail = %q, want %q", got, want)
	}
	other := filepath.Join(dir, "Data", "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if SameLocation(target, other) || SameLocation(target, "") || SameLocation("", target) {
		t.Error("two different locations compared the same")
	}
	if !SameLocation("", "") {
		t.Error("two empty paths differ")
	}

	if !caseInsensitive(t, dir) {
		t.Log("case-sensitive volume: another case is another directory, so the case checks are skipped")
		return
	}
	for _, spelling := range []string{
		filepath.Join(dir, "data", "agent-archive"),
		filepath.Join(dir, "DATA", "AGENT-ARCHIVE"),
		strings.ToLower(filepath.Join(link, "Agent-Archive")),
	} {
		if got := CanonicalPath(spelling); got != target {
			t.Errorf("CanonicalPath(%q) = %q, want %q", spelling, got, target)
		}
		if !SameLocation(spelling, target) {
			t.Errorf("SameLocation(%q, %q) = false", spelling, target)
		}
	}
}
