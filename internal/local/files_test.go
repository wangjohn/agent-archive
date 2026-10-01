package local

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateAtomicFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "state.json")
	if e := Write(p, map[string]int{"a": 1}); e != nil {
		t.Fatal(e)
	}
	var got map[string]int
	if e := Read(p, &got); e != nil {
		t.Fatal(e)
	}
	if got["a"] != 1 {
		t.Fatal(got)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

// WriteCompact writes what json.Marshal gives, and a value that fails to
// encode partway leaves the file it would have replaced untouched, with no
// temporary file behind.
func TestWriteCompactIsAtomicCompactJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	value := map[string]any{"b": []int{1, 2}, "a": "<x>"}
	if e := WriteCompact(p, value); e != nil {
		t.Fatal(e)
	}
	want, e := json.Marshal(value)
	if e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	if string(got) != string(want)+"\n" {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	if e := WriteCompact(p, map[string]any{"a": strings.Repeat("x", 1<<20), "z": make(chan int)}); e == nil {
		t.Fatal("encoded a channel")
	}
	if after, _ := os.ReadFile(p); string(after) != string(got) {
		t.Fatalf("a failed write changed the file to %q", after)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind %v", entries)
	}
}

// A staged file leaves its target alone until Commit, which gives the
// target Write's bytes and mode; Discard removes a staged file that was not
// committed, and leaves one that was.
func TestStagedFileReplacesItsTargetOnlyOnCommit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if e := Write(p, map[string]int{"a": 1}); e != nil {
		t.Fatal(e)
	}
	old, _ := os.ReadFile(p)

	discarded, e := Stage(p, map[string]int{"a": 2})
	if e != nil {
		t.Fatal(e)
	}
	if now, _ := os.ReadFile(p); string(now) != string(old) {
		t.Fatalf("staging changed the target to %q", now)
	}
	discarded.Discard()
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("Discard left behind %v", entries)
	}

	value := map[string]int{"a": 3}
	staged, e := Stage(p, value)
	if e != nil {
		t.Fatal(e)
	}
	if e := staged.Commit(); e != nil {
		t.Fatal(e)
	}
	staged.Discard()
	if e := staged.SyncDir(); e != nil {
		t.Fatal(e)
	}
	want := filepath.Join(dir, "want.json")
	if e := Write(want, value); e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(p)
	if wantBytes, _ := os.ReadFile(want); string(got) != string(wantBytes) {
		t.Fatalf("replaced with %q, want Write's %q", got, wantBytes)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	var nothing *Staged
	nothing.Discard()
}

func TestLockExcludesOtherCollector(t *testing.T) {
	home := t.TempDir()
	unlock, e := Lock(home)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Lock(home); !errors.Is(e, ErrBusy) {
		t.Fatal("second writer admitted", e)
	}
	unlock()
	unlock, e = Lock(home)
	if e != nil {
		t.Fatal(e)
	}
	unlock()
}

func TestHomeRejectsGitSymlink(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repo, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ARCHIVE_HOME", filepath.Join(root, "link", "private"))
	if _, e := Home(); e == nil {
		t.Fatal("allowed private data under Git via symlink")
	}
}

func TestNamedLockWaitRespectsDeadline(t *testing.T) {
	home := t.TempDir()
	unlock, err := NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	_, err = NamedLockWait(home, "hooks.lock", 30*time.Millisecond)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond || elapsed > time.Second {
		t.Fatalf("elapsed=%v", elapsed)
	}
}
