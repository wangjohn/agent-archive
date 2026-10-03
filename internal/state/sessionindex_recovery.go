package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"

	"github.com/wangjohn/agent-archive/internal/local"
)

const sessionIndexMarkerFile = "session-index.json"

type sessionIndexMarker struct {
	Version    int    `json:"version"`
	Complete   bool   `json:"complete"`
	Generation string `json:"generation,omitempty"`
}

func (s *Store) sessionIndexMissAllowed() error {
	var marker sessionIndexMarker
	err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || marker.Version != 1 || !marker.Complete {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

// MarkSessionIndexRecoveryNeeded records known damaged or incomplete migration
// evidence. It does not claim released old binaries understand this marker.
func (s *Store) MarkSessionIndexRecoveryNeeded() error {
	_, err := s.beginSessionIndexRecovery()
	return err
}

// Recovery requests already hold hooks.lock; retention holds collector.lock
// and may hold a request lock. This helper must not acquire hooks.lock.
func (s *Store) beginSessionIndexRecovery() (string, error) {
	generation, err := local.ID()
	if err != nil {
		return "", err
	}
	err = local.Write(filepath.Join(s.home, sessionIndexMarkerFile), sessionIndexMarker{Version: 1, Generation: generation})
	return generation, err
}

// Collector begin commits under hooks.lock so a request's marker and key
// writes finish before the census starts. Staging and sync stay outside that
// lock; the shared request/retention helper cannot take it recursively.
func (s *Store) beginCollectorSessionIndexRecovery(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	generation, err := local.ID()
	if err != nil {
		return "", err
	}
	err = s.writeUnderLock(lockedWrite{
		lock:  func() (func(), error) { return s.namedLockWait("hooks.lock", time.Second) },
		path:  filepath.Join(s.home, sessionIndexMarkerFile),
		check: ctx.Err,
		change: func(fileSnapshot) (any, bool, error) {
			return sessionIndexMarker{Version: 1, Generation: generation}, true, nil
		},
		blind: true,
	})
	return generation, err
}

func (s *Store) completeSessionIndexRecovery(generation string) error {
	return s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return local.NamedLockWait(s.home, "hooks.lock", time.Second) },
		path: filepath.Join(s.home, sessionIndexMarkerFile),
		change: func(current fileSnapshot) (any, bool, error) {
			var marker sessionIndexMarker
			if !current.found || json.Unmarshal(current.data, &marker) != nil || marker.Version != 1 || marker.Complete || marker.Generation != generation {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			marker.Complete = true
			return marker, true, nil
		},
	})
}

// RequestSessionIndexRecovery retains the damaged key so a complete census
// can durably record an absent result, without rescanning on a later hook.
// The caller holds hooks.lock; no native identity is used as a filename.
func (s *Store) RequestSessionIndexRecovery(key agentmeta.SessionKey) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		return err
	}
	if err := s.indexStep("recovery-request-marked"); err != nil {
		return err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err == nil && found && !entry.Absent {
		valid, err := s.matchingRegistration(key, entry.ArchiveSessionID)
		if err == nil && valid {
			return nil
		}
	}
	return local.Write(qualifiedSessionIndexPath(s.home, key), qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Recovery: true})
}

// RecoverSessionIndex repairs qualified indexes from one complete registration
// enumeration, outside hooks. The caller owns collector.lock. It retains an
// incomplete marker after any census or conversion failure; retry is idempotent.
// Only this census detects arbitrary duplicate registrations. Bounded lookup
// cannot discover silent loss or manually introduced duplicates elsewhere.
func (s *Store) RecoverSessionIndex(ctx context.Context) error {
	if err := s.indexStep("recovery-begin"); err != nil {
		return err
	}
	generation, err := s.beginCollectorSessionIndexRecovery(ctx)
	if err != nil {
		return err
	}
	if err := s.indexStep("recovery-incomplete"); err != nil {
		return err
	}
	inventory, err := s.sessionRegistrationInventory(ctx)
	if err != nil {
		return err
	}
	if err := s.indexStep("recovery-enumerated"); err != nil {
		return err
	}
	var failures []error
	for key, owners := range inventory {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(owners) > 1 {
			entry := qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Conflict: true}
			// Registration additions are serialized by hooks.lock for live/import
			// writers. Recovery does not hold that lock across its census or syncs.
			err := s.writeIndexUnderRequestLock(owners[0], qualifiedSessionIndexPath(s.home, key), func() error {
				for _, id := range owners {
					valid, err := s.matchingRegistration(key, id)
					if err != nil {
						return err
					}
					if !valid {
						return ErrSessionIndexRecoveryRequired
					}
				}
				return nil
			}, func(fileSnapshot) (any, bool, error) { return entry, true, nil })
			if err != nil {
				failures = append(failures, err)
			}
			continue
		}
		id := owners[0]
		err := s.writeIndexUnderRequestLock(id, qualifiedSessionIndexPath(s.home, key), func() error {
			valid, err := s.matchingRegistration(key, id)
			if err != nil {
				return err
			}
			if !valid {
				return ErrSessionIndexRecoveryRequired
			}
			return nil
		}, func(current fileSnapshot) (any, bool, error) {
			// Existing valid reservations/committed registrations belonging to another
			// archive ID may have arrived after enumeration: never overwrite them.
			if current.found {
				var prior qualifiedSessionIndexEntry
				if jsonErr := json.Unmarshal(current.data, &prior); jsonErr == nil && prior.validate(key) == nil && prior.ArchiveSessionID != id {
					valid, err := s.matchingRegistration(key, prior.ArchiveSessionID)
					if err != nil {
						return nil, false, err
					}
					if valid || prior.Reservation != "" {
						return nil, false, ErrSessionIdentityConflict
					}
				}
			}
			return indexEntry(key, id), true, nil
		})
		if err != nil {
			failures = append(failures, err)
		}
		if err := s.indexStep("recovery-entry"); err != nil {
			return err
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	if err := s.recoverCandidateIndexes(ctx); err != nil {
		return err
	}
	if err := s.recoverRequestedMisses(ctx, inventory); err != nil {
		return err
	}
	if err := s.indexStep("recovery-completing"); err != nil {
		return err
	}
	if err := s.completeSessionIndexRecovery(generation); err != nil {
		return err
	}
	return s.indexStep("recovery-complete")
}

// RecoverSessionIndexIfNeeded performs startup or requested recovery. Ordinary
// new-home misses still admit proven fresh starts without a mandatory census.
func (s *Store) RecoverSessionIndexIfNeeded(ctx context.Context) error {
	var marker sessionIndexMarker
	err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if err == nil && marker.Version == 1 && marker.Complete {
		lost, err := s.sessionIndexDirectoriesLost()
		if err != nil {
			return err
		}
		if !lost {
			return nil
		}
	}
	return s.RecoverSessionIndex(ctx)
}

// sessionIndexDirectoriesLost detects total derived-index loss even when Open
// recreated empty directories. It does not infer any individual key's absence:
// the existing complete census must restore authoritative owners first.
func (s *Store) sessionIndexDirectoriesLost() (bool, error) {
	for _, name := range []string{"sessions-v1", "sessions"} {
		present, err := directoryHasEntry(filepath.Join(s.home, name))
		if err != nil || present {
			return false, err
		}
	}
	return directoryHasEntry(filepath.Join(s.home, "registrations"))
}

// directoryHasEntry reads only one directory name, without listing or reading
// its entries. A healthy empty store therefore remains a completed no-op.
func directoryHasEntry(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("session index availability cannot be checked")
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errors.New("session index availability cannot be checked")
	}
	return len(names) > 0, nil
}

// Candidate ownership is durable reservation evidence even when a child has
// not produced a transcript/registration. Preserve its archived link identity.
func (s *Store) recoverCandidateIndexes(ctx context.Context) error {
	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil || !safeFileComponent(candidate.ArchiveSessionID) {
			return ErrSessionIndexRecoveryRequired
		}
		entry, found, err := s.readQualifiedIndex(key)
		if err != nil && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
			return err
		}
		if found && !entry.Absent {
			if entry.ArchiveSessionID != candidate.ArchiveSessionID {
				return ErrSessionIdentityConflict
			}
			continue
		}
		parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
		if err != nil {
			return err
		}
		parent, valid, err := s.LoadRegistration(candidate.ParentArchiveSessionID)
		if err != nil {
			return err
		}
		if !valid {
			continue
		} // A candidate never recreates its missing parent.
		actual, err := registrationKey(parent)
		if err != nil || actual != parentKey || parent.ArchiveSessionID != candidate.ParentArchiveSessionID {
			return ErrSessionIdentityConflict
		}
		owner, err := local.ID()
		if err != nil {
			return err
		}
		next := indexEntry(key, candidate.ArchiveSessionID)
		next.Reservation = owner
		if err := s.writeIndexUnderRequestLock(candidate.ArchiveSessionID, qualifiedSessionIndexPath(s.home, key), func() error {
			current, present, err := readJSON[SubagentCandidate](s.subagentCandidatePath(candidate.ArchiveSessionID))
			if err != nil {
				return err
			}
			if !present || current.NativeSessionID != key.NativeID || current.ParentArchiveSessionID != candidate.ParentArchiveSessionID || agentmeta.Canonical(agentmeta.Builtins(), current.Harness.Name) != string(key.Agent) {
				return ErrSessionIndexRecoveryRequired
			}
			return nil
		}, func(current fileSnapshot) (any, bool, error) {
			if current.found {
				var latest qualifiedSessionIndexEntry
				if err := json.Unmarshal(current.data, &latest); err == nil && latest.validate(key) == nil && !latest.Absent {
					return nil, false, ErrSessionIndexRecoveryRequired
				}
			}
			return next, true, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverRequestedMisses(ctx context.Context, inventory map[agentmeta.SessionKey][]string) error {
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.home, "sessions-v1", file.Name())
		var entry qualifiedSessionIndexEntry
		if err := local.Read(path, &entry); err != nil {
			return ErrSessionIndexRecoveryRequired
		}
		if !entry.Recovery {
			continue
		}
		key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
		if err := key.Validate(); err != nil {
			return err
		}
		if path != qualifiedSessionIndexPath(s.home, key) || entry.Version != 1 {
			return ErrSessionIndexRecoveryRequired
		}
		if len(inventory[key]) != 0 {
			return ErrSessionIndexRecoveryRequired
		}
		// Capture/import registration is serialized by hooks.lock. This narrow
		// application lock never covers the inventory enumeration or disk sync.
		err := s.writeUnderLock(lockedWrite{
			lock: func() (func(), error) { return local.NamedLockWait(s.home, "hooks.lock", time.Second) },
			path: path,
			change: func(current fileSnapshot) (any, bool, error) {
				var latest qualifiedSessionIndexEntry
				if err := json.Unmarshal(current.data, &latest); err != nil {
					return nil, false, ErrSessionIndexRecoveryRequired
				}
				if !latest.Recovery {
					return nil, false, nil
				}
				return qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Absent: true}, true, nil
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sessionRegistrationInventory(ctx context.Context) (map[agentmeta.SessionKey][]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.home, "registrations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("recover session identities: %w", err)
	}
	inventory := make(map[agentmeta.SessionKey][]string, len(entries))
	for _, file := range entries {
		if filepath.Ext(file.Name()) == quarantineSuffix {
			return nil, ErrSessionIndexRecoveryRequired
		}
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reg, found, err := s.LoadRegistration(id)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrSessionIndexRecoveryRequired
		}
		key, err := registrationKey(reg)
		if err != nil || reg.Validate() != nil || reg.ArchiveSessionID != id || !safeFileComponent(id) {
			return nil, fmt.Errorf("invalid registration identity %q: %w", id, ErrSessionIndexRecoveryRequired)
		}
		inventory[key] = append(inventory[key], id)
	}
	return inventory, nil
}
