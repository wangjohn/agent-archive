package local

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoveStaleTempsRemovesOnlyOldTemporaries(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, tempPrefix+"stale")
	fresh := filepath.Join(dir, tempPrefix+"fresh")
	other := filepath.Join(dir, "state.json")
	nested := filepath.Join(dir, "sub", tempPrefix+"stale")
	if err := os.MkdirAll(filepath.Dir(nested), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stale, fresh, other, nested} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if path != fresh {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := RemoveStaleTemps(dir, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temporary survived: %v", err)
	}
	for _, path := range []string{fresh, other, nested} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
	}
	if err := RemoveStaleTemps(filepath.Join(dir, "missing"), time.Hour); err != nil {
		t.Fatalf("missing directory: %v", err)
	}
}

func TestTrimLogKeepsRecentLinesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector-error.log")
	var log bytes.Buffer
	for i := 0; log.Len() < 4096; i++ {
		log.WriteString(strings.Repeat("x", 30) + "\n")
	}
	log.WriteString("last line\n")
	if err := os.WriteFile(path, log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	// A writer with the file open for appending, as launchd leaves stderr.
	appender, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer appender.Close()
	before, _ := os.Stat(path)

	if err := TrimLog(path, 1024, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := appender.WriteString("after trim\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 100+len("after trim\n") || !strings.HasSuffix(string(got), "last line\nafter trim\n") || !strings.HasPrefix(string(got), "x") {
		t.Fatalf("trimmed log = %q", got)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("the log was replaced, not trimmed in place")
	}

	// Under the limit, and missing, it is left alone.
	if err := TrimLog(path, 1024, 100); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, got) {
		t.Fatal("a log under the limit was changed")
	}
	if err := TrimLog(filepath.Join(t.TempDir(), "missing.log"), 1024, 100); err != nil {
		t.Fatal(err)
	}
}
