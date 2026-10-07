package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// hasPublicationEvidence observes the confined directory without reading bodies.
// Any entry is owed, including malformed journals and interrupted atomic files.
func (s *Store) hasPublicationEvidence(id string) (owed bool, err error) {
	if _, err = s.evidencePath(id); err != nil {
		return true, err
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, root.Close())
		if err != nil {
			owed = true
		}
	}()
	dir, err := root.Open(filepath.Join(publicationEvidenceDir, id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, dir.Close())
		if err != nil {
			owed = true
		}
	}()
	entries, err := dir.ReadDir(1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return true, err
	}
	return len(entries) > 0, nil
}
