package rolloutcatalog

import (
	"io/fs"
	"os"
	"path/filepath"
)

func measureStat(c *Counters) {
	if c != nil {
		c.Stats++
	}
}

func measureRootOpen(c *Counters) {
	if c != nil {
		c.RootOpens++
	}
}

func measureFileOpen(c *Counters) {
	if c != nil {
		c.FileOpens++
	}
}

func measureResolution(c *Counters) {
	if c != nil {
		c.Resolutions++
	}
}

func measuredLstat(c *Counters, root, path string) (fs.FileInfo, error) {
	measureRootOpen(c)
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opened.Close() }()
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	measureStat(c)
	return opened.Lstat(relative)
}

type measuredFile struct {
	*os.File
	counters *Counters
}

func (f measuredFile) Stat() (fs.FileInfo, error) { measureStat(f.counters); return f.File.Stat() }

func (f measuredFile) ReadAt(p []byte, offset int64) (int, error) {
	n, err := f.File.ReadAt(p, offset)
	if f.counters != nil {
		f.counters.FileBytes += int64(n)
	}
	return n, err
}
