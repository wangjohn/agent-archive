// Package setupjournal is setup's transaction record: the journal setup
// writes before it changes any hook file or the LaunchAgent, which recovery
// reads to put an interrupted setup back, and which every other command
// checks to see that one is pending.
package setupjournal

import (
	"os"
	"path/filepath"
)

// JournalPath is where setup records its transaction in the data directory home.
func JournalPath(home string) string { return filepath.Join(home, "setup-transaction.json") }

// TransactionPending reports whether an interrupted or running setup's
// journal exists (or cannot be checked) in home.
func TransactionPending(home string) bool {
	_, err := os.Stat(JournalPath(home))
	return !os.IsNotExist(err)
}
