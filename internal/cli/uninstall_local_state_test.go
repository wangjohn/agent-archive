package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
)

// Every entry the collector's local store can create under the data
// directory is one uninstall --delete-local-data removes. The list comes from
// the store itself, so a directory it adds later cannot be left behind.
func TestDeleteLocalDataRemovesEveryLocalStoreEntry(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agent-archive")
	for _, entry := range state.OwnedEntries() {
		path := filepath.Join(home, entry)
		if strings.HasSuffix(entry, ".json") {
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	leftover, err := removeLocalState(home)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("leftover = %v, err = %v: add the entry to localStateEntries", leftover, err)
	}
	// The directory itself goes once uninstall has released its locks.
	if remaining, err := os.ReadDir(home); err != nil || len(remaining) != 0 {
		t.Fatalf("data directory still holds %v (%v)", remaining, err)
	}
}

// handListedLocalState is localStateEntries as it was before uninstall took
// the store's entries from state.OwnedEntries, when every name was listed by
// hand. uninstall --delete-local-data removes exactly these, no more and no
// fewer; an entry added to either list later is added here too, on purpose.
var handListedLocalState = []string{
	"config.json", "setup-draft.json", "setup-transaction.json",
	"registrations", "requests", "request-locks", "published", "pending", "sessions", "superseded", "pending-scans", "scan-signatures", "subagent-candidates", "forgotten", "refresh-skips", "imports",
	"status.json", "storage-clock.json", "storage-health.json", "capture-diagnostics.json", "diagnostics.lock", "application-versions.json",
	"collector.lock", "collector-lock.json", "collector.log", "collector-error.log",
	"cache", "handoffs",
}

// The entries uninstall deletes, the store's now taken from
// state.OwnedEntries, are the set it deleted when all were listed by hand.
func TestDeleteLocalDataListIsTheHandListedOne(t *testing.T) {
	got := slices.Compact(slices.Sorted(slices.Values(append(state.OwnedEntries(), localStateEntries...))))
	want := slices.Compact(slices.Sorted(slices.Values(handListedLocalState)))
	if !slices.Equal(got, want) {
		t.Fatalf("uninstall deletes %q\nwant %q", got, want)
	}
}

// removeLocalState removes every entry agent-archive makes, the files moved
// aside from them, and write temp files; it keeps the lock files uninstall
// still holds, and leaves a user's own files in place and reported, including
// ones named like agent-archive's but not made by it.
func TestDeleteLocalDataRemovesOnlyItsOwnEntries(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agent-archive")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range handListedLocalState {
		if !strings.Contains(name, ".") {
			if err := os.MkdirAll(filepath.Join(home, name, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(name)
		if name != "collector.lock" {
			write(name + ".20260925T010203Z.corrupt")
		}
	}
	write(".pending-123")
	locks := []string{"setup.lock", "hooks.lock"}
	for _, name := range locks {
		write(name)
	}
	user := []string{"my-notes.txt", "status.json.bak", "registrations-old", "cache2", "notes.corrupt", "stranger.json.20260925T010203Z.corrupt"}
	for _, name := range user {
		write(name)
	}

	leftover, err := removeLocalState(home)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(leftover)
	if want := slices.Sorted(slices.Values(user)); !slices.Equal(leftover, want) {
		t.Fatalf("leftover %q, want %q", leftover, want)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	remaining := []string{}
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}
	want := slices.Sorted(slices.Values(append(append([]string{"collector.lock"}, locks...), user...)))
	if !slices.Equal(remaining, want) {
		t.Fatalf("data directory holds %q, want %q", remaining, want)
	}
}
