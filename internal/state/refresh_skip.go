package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
)

// RefreshSkip records that the current parser cannot refresh a session's
// last publication's metadata, so the collector stops retrying it. It names
// no content: only the parser version, the published source's object key,
// and why.
type RefreshSkip struct {
	ParserVersion string            `json:"parser_version"`
	SourceKey     string            `json:"source_key"`
	Reason        RefreshSkipReason `json:"reason"`
}

// RefreshSkipReason says why a metadata refresh cannot be done.
type RefreshSkipReason string

const (
	// RefreshSkipUnderivable means this build cannot derive metadata from the
	// retained bundle at all.
	RefreshSkipUnderivable RefreshSkipReason = "metadata_underivable"
	// RefreshSkipSourceUnavailable means the recorded source is missing from
	// storage, or differs from its record, and this build cannot rebuild it.
	RefreshSkipSourceUnavailable RefreshSkipReason = "source_unavailable"
)

const refreshSkipDir = "refresh-skips"

func (s *Store) refreshSkipPath(id string) string {
	return filepath.Join(s.home, refreshSkipDir, id+".json")
}

// SaveRefreshSkip records that a session's metadata refresh cannot be done.
func (s *Store) SaveRefreshSkip(id string, skip RefreshSkip) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	return local.Write(s.refreshSkipPath(id), skip)
}

// LoadRefreshSkip returns a session's refresh-skip record. One that cannot
// be read is treated as absent: the refresh is then simply tried again.
func (s *Store) LoadRefreshSkip(id string) (RefreshSkip, bool, error) {
	if !safeFileComponent(id) {
		return RefreshSkip{}, false, errors.New("archive session ID is not a safe file name component")
	}
	skip, found := readRefreshSkip(s.refreshSkipPath(id))
	return skip, found, nil
}

// readRefreshSkip decodes the refresh-skip record at path; found is false
// when it cannot be read (see LoadRefreshSkip).
func readRefreshSkip(path string) (skip RefreshSkip, found bool) {
	if err := local.Read(path, &skip); err != nil {
		return RefreshSkip{}, false
	}
	return skip, true
}

// RemoveRefreshSkip drops a session's record once a publication makes it
// stale.
func (s *Store) RemoveRefreshSkip(id string) error {
	err := os.Remove(s.refreshSkipPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove refresh-skip record %q: %w", id, err)
	}
	return nil
}

// CountRefreshSkips counts the sessions whose metadata parserVersion cannot
// refresh (see Status.UnrefreshableSummaries).
func (s *Store) CountRefreshSkips(parserVersion string) int {
	entries, err := os.ReadDir(filepath.Join(s.home, refreshSkipDir))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var skip RefreshSkip
		if local.Read(filepath.Join(s.home, refreshSkipDir, entry.Name()), &skip) == nil && skip.ParserVersion == parserVersion {
			count++
		}
	}
	return count
}
