//go:build !darwin && !linux

package state

import (
	"os"
	"strconv"
)

// Quota authority is path/stat/physical size only; publication still performs
// full validation. Unix targets additionally bind the native device/inode.
func pendingQuotaIdentity(info os.FileInfo) string {
	return info.Name() + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}
