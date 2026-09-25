package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
)

// A native session ID is harness-controlled input and must never be used
// directly as a file name component (it could contain path separators or
// arbitrary bytes); its hash is a safe, stable index key instead.
func nativeSessionIndexPath(home, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(nativeSessionID))
	return filepath.Join(home, "sessions", hex.EncodeToString(sum[:])+".json")
}

type sessionIndexEntry struct {
	ArchiveSessionID string `json:"archive_session_id"`
}

// ArchiveSessionID returns the persistent archive session ID previously
// associated with a native session ID, if one has been recorded.
func (s *Store) ArchiveSessionID(nativeSessionID string) (string, bool, error) {
	if strings.TrimSpace(nativeSessionID) == "" {
		return "", false, errors.New("native session ID is required")
	}
	var entry sessionIndexEntry
	err := local.Read(nativeSessionIndexPath(s.home, nativeSessionID), &entry)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	case IsUndecodable(err) || (err == nil && !safeFileComponent(entry.ArchiveSessionID)):
		// The index is derived from the registrations, so an entry that no
		// longer decodes is recovered from the registration naming this
		// native session (rebuiltFromRegistrations). Without one there is
		// nothing to recover, and EnsureArchiveSessionID assigns a fresh ID.
		return s.indexFromRegistrations(nativeSessionID)
	case err != nil:
		return "", false, fmt.Errorf("read session index: %w", err)
	}
	return entry.ArchiveSessionID, true, nil
}

// indexFromRegistrations finds the archive session ID registered for a
// native session by reading the registrations, for an index entry that no
// longer decodes. Registrations that cannot be read are passed over.
func (s *Store) indexFromRegistrations(nativeSessionID string) (string, bool, error) {
	ids, err := s.listJSONStems("registrations")
	if err != nil {
		return "", false, fmt.Errorf("recover session index: %w", err)
	}
	for _, id := range ids {
		reg, found, err := readJSON[struct {
			ArchiveSessionID string `json:"archive_session_id"`
			NativeSessionID  string `json:"native_session_id"`
		}](s.registrationPath(id))
		if err == nil && found && reg.NativeSessionID == nativeSessionID && reg.ArchiveSessionID == id {
			return id, true, nil
		}
	}
	return "", false, nil
}

// EnsureArchiveSessionID finds the archive session ID already associated
// with nativeSessionID, or creates and durably records a fresh random one.
// It is the "creates or finds a persistent random archive session ID" step
// the spec assigns to a start/resume hook.
func (s *Store) EnsureArchiveSessionID(nativeSessionID string) (id string, created bool, err error) {
	existing, found, err := s.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return "", false, err
	}
	if found {
		// Rewritten when it was recovered rather than read: a no-op write
		// otherwise costs nothing but a comparison.
		var entry sessionIndexEntry
		if local.Read(nativeSessionIndexPath(s.home, nativeSessionID), &entry) != nil || entry.ArchiveSessionID != existing {
			if err := local.Write(nativeSessionIndexPath(s.home, nativeSessionID), sessionIndexEntry{ArchiveSessionID: existing}); err != nil {
				return "", false, fmt.Errorf("repair session index: %w", err)
			}
		}
		return existing, false, nil
	}
	fresh, err := local.ID()
	if err != nil {
		return "", false, fmt.Errorf("generate archive session ID: %w", err)
	}
	if err := local.Write(nativeSessionIndexPath(s.home, nativeSessionID), sessionIndexEntry{ArchiveSessionID: fresh}); err != nil {
		return "", false, fmt.Errorf("write session index: %w", err)
	}
	return fresh, true, nil
}
