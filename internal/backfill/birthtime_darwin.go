//go:build darwin

package backfill

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime returns when the file was created. APFS and HFS+ record it;
// a copy or restore resets it, which started_at_source file_created admits.
func fileBirthTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && (st.Birthtimespec.Sec != 0 || st.Birthtimespec.Nsec != 0) {
		return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec), nil
	}
	return info.ModTime(), nil
}
