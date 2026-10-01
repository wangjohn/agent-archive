package sourcefacts

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// RootOpener preserves root-descriptor confinement when using verified snapshots.
type RootOpener struct{ Root string }

// Lstat observes a confined locator without following its final symlink.
func (o RootOpener) Lstat(path string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(o.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	relative, err := filepath.Rel(o.Root, path)
	if err != nil {
		return nil, err
	}
	return root.Lstat(relative)
}

// EvalSymlinks supplies canonical containment facts to the snapshot policy.
func (o RootOpener) EvalSymlinks(path string) (string, error) { return filepath.EvalSymlinks(path) }

// OpenRegular opens through the approved root descriptor.
func (o RootOpener) OpenRegular(path string) (transcriptio.File, error) {
	return OpenRegular(o.Root, path)
}
