package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
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
		return s.indexFromRegistrations(nativeSessionID)
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
	foundID := ""
	for _, id := range ids {
		reg, found, e := s.LoadRegistration(id)
		if e == nil && found && reg.NativeSessionID == nativeSessionID && reg.ArchiveSessionID == id {
			if foundID != "" && foundID != id {
				return "", false, errors.New("ambiguous native identity; specify agent")
			}
			foundID = id
		}
	}
	if foundID != "" {
		return foundID, true, nil
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

// AgentSessionID looks up a native identity in its agent namespace. Legacy
// mappings migrate only after the referenced registration proves ownership.
// Callers serialize admission with hooks.lock.
func (s *Store) AgentSessionID(agent, native string) (string, bool, error) {
	agent = archive.CanonicalHarness(agent)
	if agent == "" || strings.TrimSpace(native) == "" {
		return "", false, errors.New("agent and native identity required")
	}
	path := nativeSessionIndexPath(s.home, agent+"\x00"+native)
	var entry sessionIndexEntry
	err := local.Read(path, &entry)
	if err == nil {
		if !safeFileComponent(entry.ArchiveSessionID) {
			return "", false, errors.New("identity index needs repair")
		}
		reg, found, e := s.LoadRegistration(entry.ArchiveSessionID)
		if e != nil {
			return "", false, e
		}
		if found && (archive.CanonicalHarness(reg.Harness.Name) != agent || reg.NativeSessionID != native) {
			return "", false, errors.New("identity index conflict")
		}
		return entry.ArchiveSessionID, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, errors.New("identity index needs repair")
	}
	if id, found, e := s.journalLookup(agent, native); e != nil || found {
		return id, found, e
	}
	var legacy sessionIndexEntry
	err = local.Read(nativeSessionIndexPath(s.home, native), &legacy)
	if err == nil && !safeFileComponent(legacy.ArchiveSessionID) {
		return "", false, errors.New("legacy identity index needs repair")
	}
	if err == nil {
		reg, found, e := s.LoadRegistration(legacy.ArchiveSessionID)
		if e != nil {
			return "", false, e
		}
		if found && reg.NativeSessionID != native {
			return "", false, errors.New("legacy identity index ownership conflict")
		}
		if found && archive.CanonicalHarness(reg.Harness.Name) == agent && reg.NativeSessionID == native {
			return legacy.ArchiveSessionID, true, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false, errors.New("legacy identity index needs repair")
	}
	return "", false, nil
}

// EnsureAgentSessionID allocates an identity in an agent namespace.
func (s *Store) EnsureAgentSessionID(agent, native string) (string, bool, error) {
	if err := config.ProtectIdentityWriter(s.home); err != nil {
		return "", false, err
	}
	if err := s.ensureIdentityReady(); err != nil {
		return "", false, err
	}
	id, found, err := s.AgentSessionID(agent, native)
	if err != nil {
		return id, false, err
	}
	if found {
		if err := s.journalIdentity(agent, native, id); err != nil {
			return "", false, err
		}
		path := nativeSessionIndexPath(s.home, archive.CanonicalHarness(agent)+"\x00"+native)
		var entry sessionIndexEntry
		if e := local.Read(path, &entry); errors.Is(e, os.ErrNotExist) {
			if e := local.Write(path, sessionIndexEntry{ArchiveSessionID: id}); e != nil {
				return "", false, e
			}
		}
		return id, false, nil
	}
	id, err = local.ID()
	if err != nil {
		return "", false, err
	}
	if err := s.journalIdentity(agent, native, id); err != nil {
		return "", false, err
	}
	err = local.Write(nativeSessionIndexPath(s.home, archive.CanonicalHarness(agent)+"\x00"+native), sessionIndexEntry{ArchiveSessionID: id})
	return id, true, err
}
