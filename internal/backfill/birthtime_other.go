//go:build !darwin

package backfill

import (
	"os"
	"time"
)

// fileBirthTime falls back to the modification time where the standard
// library exposes no birth time (Linux: ext4, btrfs and xfs record one, but
// reading it takes statx, which is not used here).
//
// Backfill uses a Cursor transcript's birth time as the chat's start time
// (started_at_source file_created). With this fallback the start time on
// Linux is when the transcript was last written, so a chat that was resumed
// sorts at its last activity instead of its beginning, and --since/--until
// (which compare the start time) can include or leave out a resumed chat
// that the same history backfilled on a Mac would not. The two never
// disagree about which chats exist, only about the start time of resumed
// ones.
func fileBirthTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
