package state

import (
	"io"
	"os"
	"syscall"
)

func identityDirBatch(f *os.File, offset int64, count int) ([]string, int64, bool, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	// One kernel record cannot exceed NAME_MAX + dirent metadata. Keep the
	// batch below the requested bound without dropping buffered entries.
	bytes := min(max(count*24, 280), 4096)
	buffer := make([]byte, bytes)
	n, err := syscall.ReadDirent(int(f.Fd()), buffer)
	if err != nil {
		return nil, 0, false, err
	}
	_, _, names := syscall.ParseDirent(buffer[:n], -1, nil)
	next, err := f.Seek(0, io.SeekCurrent)
	return names, next, n == 0, err
}
