package cli

import (
	"os"
	"path/filepath"
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
