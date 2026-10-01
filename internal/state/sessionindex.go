package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrSessionIndexRecoveryRequired asks the collector to repair derived identity
// bookkeeping. Hooks never enumerate registrations to resolve it.
var ErrSessionIndexRecoveryRequired = errors.New("session index recovery is required")

// ErrSessionIdentityConflict reports incompatible owners or a known duplicate.
var ErrSessionIdentityConflict = errors.New("session identity has conflicting registrations")

func nativeSessionIndexPath(home, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(nativeSessionID))
	return filepath.Join(home, "sessions", hex.EncodeToString(sum[:])+".json")
}

func qualifiedSessionIndexPath(home string, key agentmeta.SessionKey) string {
	sum := sha256.Sum256(key.Encoding())
	return filepath.Join(home, "sessions-v1", hex.EncodeToString(sum[:])+".json")
}

type sessionIndexEntry struct {
	ArchiveSessionID string `json:"archive_session_id"`
}

type qualifiedSessionIndexEntry struct {
	Version          int          `json:"version"`
	Agent            agentmeta.ID `json:"agent"`
	NativeID         string       `json:"native_id"`
	ArchiveSessionID string       `json:"archive_session_id,omitempty"`
	// Reservation owns an unfinished ID, distinct from a committed index whose
	// registration disappeared. A durable registration wins even before cleanup.
	Reservation string `json:"reservation,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
	Recovery    bool   `json:"recovery,omitempty"`
	Absent      bool   `json:"absent,omitempty"`
}

func indexEntry(key agentmeta.SessionKey, id string) qualifiedSessionIndexEntry {
	return qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, ArchiveSessionID: id}
}

func (e qualifiedSessionIndexEntry) validate(key agentmeta.SessionKey) error {
	if e.Version != 1 || e.Agent != key.Agent || e.NativeID != key.NativeID {
		return ErrSessionIndexRecoveryRequired
	}
	if e.Recovery {
		return ErrSessionIndexRecoveryRequired
	}
	if e.Absent {
		if e.ArchiveSessionID != "" || e.Reservation != "" || e.Conflict {
			return ErrSessionIndexRecoveryRequired
		}
		return nil
	}
	if e.Conflict {
		return ErrSessionIdentityConflict
	}
	if !safeFileComponent(e.ArchiveSessionID) || (e.Reservation != "" && !safeFileComponent(e.Reservation)) {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (s *Store) readQualifiedIndex(key agentmeta.SessionKey) (qualifiedSessionIndexEntry, bool, error) {
	var entry qualifiedSessionIndexEntry
	err := local.Read(qualifiedSessionIndexPath(s.home, key), &entry)
	if errors.Is(err, os.ErrNotExist) {
		return entry, false, nil
	}
	if IsUndecodable(err) {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	if err != nil {
		return entry, false, fmt.Errorf("read qualified session index: %w", err)
	}
	if err := entry.validate(key); err != nil {
		return entry, false, err
	}
	return entry, true, nil
}

func registrationKey(reg archive.SessionRegistration) (agentmeta.SessionKey, error) {
	return agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
}

func (s *Store) matchingRegistration(key agentmeta.SessionKey, id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, ErrSessionIndexRecoveryRequired
	}
	reg, found, err := s.LoadRegistration(id)
	if IsUndecodable(err) {
		return false, ErrSessionIndexRecoveryRequired
	}
	if err != nil || !found {
		return false, err
	}
	if err := reg.Validate(); err != nil {
		return false, ErrSessionIndexRecoveryRequired
	}
	actual, err := registrationKey(reg)
	if err != nil || actual != key || reg.ArchiveSessionID != id {
		return false, ErrSessionIdentityConflict
	}
	return true, nil
}

// legacySessionID reads one historical entry and validates its owner. A legacy
// collision belonging to another agent is a miss, never that agent's ID.
func (s *Store) legacySessionID(key agentmeta.SessionKey) (string, bool, error) {
	var entry sessionIndexEntry
	err := local.Read(nativeSessionIndexPath(s.home, key.NativeID), &entry)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if IsUndecodable(err) || (err == nil && !safeFileComponent(entry.ArchiveSessionID)) {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil {
		return "", false, fmt.Errorf("read legacy session index: %w", err)
	}
	reg, found, err := s.LoadRegistration(entry.ArchiveSessionID)
	if IsUndecodable(err) {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil || !found {
		return "", false, err
	}
	actual, err := registrationKey(reg)
	if reg.Validate() != nil {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if err != nil || reg.ArchiveSessionID != entry.ArchiveSessionID {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	if actual.Agent != key.Agent {
		return "", false, nil
	}
	if actual.NativeID != key.NativeID {
		return "", false, ErrSessionIdentityConflict
	}
	return entry.ArchiveSessionID, true, nil
}

// ArchiveSessionID returns only a validated registration, reading at most one
// qualified entry, one legacy entry and their directly referenced registration.
// It performs no writes, enumeration or adoption outside the writer's locks.
func (s *Store) ArchiveSessionID(key agentmeta.SessionKey) (string, bool, error) {
	if err := key.Validate(); err != nil {
		return "", false, err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return "", false, err
	}
	if found && entry.Absent {
		return "", false, s.sessionIndexMissAllowed()
	}
	if found {
		registered, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err != nil || !registered {
			return "", false, err
		}
		return entry.ArchiveSessionID, true, nil
	}
	id, found, err := s.legacySessionID(key)
	if err != nil || found {
		return id, found, err
	}
	if err := s.sessionIndexMissAllowed(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// EnsureArchiveSessionID reserves an ID for a proven fresh start, import or
// child candidate. A reservation is not proof of an admitted registration.
// Callers hold hooks.lock, or the collector lock for child materialization.
func (s *Store) EnsureArchiveSessionID(key agentmeta.SessionKey) (id string, created bool, err error) {
	if err := key.Validate(); err != nil {
		return "", false, err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return "", false, err
	}
	if found && !entry.Absent {
		registered, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err != nil {
			return "", false, err
		}
		if registered || entry.Reservation != "" {
			return entry.ArchiveSessionID, false, nil
		}
		// A committed registration disappeared; its old ID must not be revived.
	}
	if !found {
		legacy, adopted, err := s.legacySessionID(key)
		if err != nil {
			return "", false, err
		}
		if adopted {
			err := s.writeIndexUnderRequestLock(legacy, qualifiedSessionIndexPath(s.home, key), func() error {
				valid, err := s.matchingRegistration(key, legacy)
				if err != nil {
					return err
				}
				if !valid {
					return errIndexMoved
				}
				return nil
			}, func(current fileSnapshot) (any, bool, error) {
				if current.found {
					return nil, false, errIndexMoved
				}
				return indexEntry(key, legacy), true, nil
			})
			if err != nil {
				return "", false, err
			}
			if err := s.indexStep("adoption"); err != nil {
				return "", false, err
			}
			return legacy, false, nil
		}
	}
	if err := s.sessionIndexMissAllowed(); err != nil {
		return "", false, err
	}
	fresh, err := local.ID()
	if err != nil {
		return "", false, fmt.Errorf("generate archive session ID: %w", err)
	}
	owner, err := local.ID()
	if err != nil {
		return "", false, err
	}
	next := indexEntry(key, fresh)
	next.Reservation = owner
	// A stale entry is guarded by its old request lock so expiry cannot remove a
	// replacement after our comparison. Fresh absent keys are serialized by hooks.
	lockID := fresh
	if found && !entry.Absent {
		lockID = entry.ArchiveSessionID
	}
	err = s.writeIndexUnderRequestLock(lockID, qualifiedSessionIndexPath(s.home, key), nil, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var existing qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &existing); err != nil {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			if err := existing.validate(key); err != nil {
				return nil, false, err
			}
			if !found || existing != entry {
				return nil, false, errIndexMoved
			}
			if existing.Absent {
				return next, true, nil
			}
			valid, err := s.matchingRegistration(key, existing.ArchiveSessionID)
			if err != nil {
				return nil, false, err
			}
			if valid || existing.Reservation != "" {
				return nil, false, errIndexMoved
			}
		} else if found {
			return nil, false, errIndexMoved
		}
		return next, true, nil
	})
	if err != nil {
		return "", false, err
	}
	if err := s.indexStep("reservation"); err != nil {
		return "", false, err
	}
	return fresh, true, nil
}

func (s *Store) finishSessionIndex(key agentmeta.SessionKey, id string) error {
	err := s.writeIndexUnderRequestLock(id, qualifiedSessionIndexPath(s.home, key), func() error {
		valid, err := s.matchingRegistration(key, id)
		if err != nil {
			return err
		}
		if !valid {
			return ErrSessionNotRegistered
		}
		return nil
	}, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var entry qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &entry); err != nil {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			if err := entry.validate(key); err != nil {
				return nil, false, err
			}
			if entry.ArchiveSessionID != id {
				return nil, false, ErrSessionIdentityConflict
			}
			if entry.Reservation == "" {
				return nil, false, nil
			}
		}
		return indexEntry(key, id), true, nil
	})
	if err != nil {
		return err
	}
	return s.indexStep("committed")
}

// removeSessionIndex runs under the removed archive ID's request lock. Neither
// namespace is deleted when another registration replaced its mapping.
func (s *Store) removeSessionIndex(key agentmeta.SessionKey, id string) error {
	paths := []string{qualifiedSessionIndexPath(s.home, key), nativeSessionIndexPath(s.home, key.NativeID)}
	for i, path := range paths {
		var entry sessionIndexEntry
		if i == 0 {
			var qualified qualifiedSessionIndexEntry
			err := local.Read(path, &qualified)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if qualified.Agent != key.Agent || qualified.NativeID != key.NativeID {
				continue
			}
			entry.ArchiveSessionID = qualified.ArchiveSessionID
		} else {
			err := local.Read(path, &entry)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
		}
		if entry.ArchiveSessionID != id {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) indexStep(step string) error {
	if s.onIndexStep != nil {
		return s.onIndexStep(step)
	}
	return nil
}

func (s *Store) writeIndexUnderRequestLock(id, path string, check func() error, change func(fileSnapshot) (any, bool, error)) error {
	indexStore := *s
	indexStore.onWriteSync = s.onIndexSync
	return indexStore.writeUnderRequestLock(id, path, check, change)
}
