package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

const sessionRecoveryCursorFile = "session-index-recovery.json"

// SessionIndexRecoverySlice is the scheduled recovery allowance. Enumeration
// remains complete on each slice; the expensive derived-index application resumes.
const SessionIndexRecoverySlice = 4 * time.Second

// SessionIndexRecoveryInterrupted reports ordinary cancellation or expiry,
// including wrappers, only when no checkpoint or other failure is joined to it.
func SessionIndexRecoveryInterrupted(err error) bool {
	return sessionIndexRecoveryInterrupted(err, 0)
}

func sessionIndexRecoveryInterrupted(err error, depth int) bool {
	if err == nil || depth >= 32 {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !sessionIndexRecoveryInterrupted(cause, depth+1) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return sessionIndexRecoveryInterrupted(cause, depth+1)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type sessionRecoveryCursor struct {
	Checksum       string `json:"checksum"`
	Version        int    `json:"version"`
	Generation     string `json:"generation"`
	Revision       string `json:"revision,omitempty"`
	Inventory      string `json:"inventory"`
	Phase          int    `json:"phase"`
	Offset         int    `json:"offset"`
	PhaseInventory string `json:"phase_inventory,omitempty"`
}

func inventoryFingerprint(inventory map[agentmeta.SessionKey][]string) ([]agentmeta.SessionKey, string) {
	keys := make([]agentmeta.SessionKey, 0, len(inventory))
	for key := range inventory {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Agent != keys[j].Agent {
			return keys[i].Agent < keys[j].Agent
		}
		return keys[i].NativeID < keys[j].NativeID
	})
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, key := range keys {
		_ = encoder.Encode(key)
		_ = encoder.Encode(inventory[key])
	}
	return keys, hex.EncodeToString(h.Sum(nil))
}

func phaseFingerprint(value any) string {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	switch values := value.(type) {
	case []SubagentCandidate:
		for _, candidate := range values {
			_ = encoder.Encode(candidate)
		}
	case []os.DirEntry:
		for _, entry := range values {
			_ = encoder.Encode(entry.Name())
		}
	case []string:
		for _, name := range values {
			_ = encoder.Encode(name)
		}
	default:
		_ = encoder.Encode(value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecoverSessionIndexScheduled resumes one bounded application slice. false
// means more work remains and is never an absence certificate. The caller owns
// collector.lock; requests and registration mutations may still run concurrently.
func (s *Store) RecoverSessionIndexScheduled(ctx context.Context, allowance time.Duration) (bool, error) {
	if allowance <= 0 || allowance > SessionIndexRecoverySlice {
		return false, errors.New("session index recovery allowance must be positive and at most four seconds")
	}
	return s.recoverSessionIndexSlice(ctx, allowance)
}

func (s *Store) recoverSessionIndexSlice(ctx context.Context, allowance time.Duration) (bool, error) {
	cursor, complete, err := s.prepareRecoveryCursor(ctx)
	if err != nil || complete {
		return complete, err
	}
	revision, err := s.ensureSessionMembershipRevision()
	if err != nil {
		return false, err
	}
	inventory, err := s.sessionRegistrationInventory(ctx)
	if err != nil {
		return false, err
	}
	keys, fingerprint := inventoryFingerprint(inventory)
	if err := s.indexStep("recovery-enumerated"); err != nil {
		return false, err
	}
	if cursor.Revision != revision || cursor.Inventory != fingerprint {
		cursor.Revision, cursor.Inventory = revision, fingerprint
		cursor.Phase, cursor.Offset, cursor.PhaseInventory = 0, 0, ""
	}
	// Complete registration validation is mandatory before application. Charge
	// one aggregate application allowance after that census; the caller's
	// context independently bounds the whole recovery stage.
	deadline := time.Now().Add(allowance)
	if cursor.Phase == 0 {
		if cursor.Offset > len(keys) {
			cursor.Offset = 0
		}
		complete, err := s.applyRecoveryPhase(ctx, &cursor, deadline, len(keys), func(i int) error { return s.recoverRegistrationOwners(keys[i], inventory[keys[i]]) })
		if err != nil || !complete {
			return false, err
		}
		cursor.Phase, cursor.Offset = 1, 0
	}
	// Requested keys may refer to an already-applied candidate whose index
	// changed after its checkpoint. Keep complete candidate authority available
	// in phase two, including slices that resume there directly.
	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return false, err
	}
	if cursor.Phase == 1 {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ArchiveSessionID < candidates[j].ArchiveSessionID })
		fingerprint := phaseFingerprint(candidates)
		if cursor.PhaseInventory != fingerprint || cursor.Offset > len(candidates) {
			cursor.Offset = 0
		}
		cursor.PhaseInventory = fingerprint
		complete, err := s.applyRecoveryPhase(ctx, &cursor, deadline, len(candidates), func(i int) error { return s.recoverCandidateIndex(ctx, candidates[i]) })
		if err != nil || !complete {
			return false, err
		}
		cursor.Phase, cursor.Offset, cursor.PhaseInventory = 2, 0, ""
	}
	candidateInventory := make(map[agentmeta.SessionKey][]SubagentCandidate, len(candidates))
	for _, candidate := range candidates {
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil {
			return false, err
		}
		candidateInventory[key] = append(candidateInventory[key], candidate)
	}
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	fingerprint = phaseFingerprint(entries)
	if cursor.PhaseInventory != fingerprint || cursor.Offset > len(entries) {
		cursor.Offset = 0
	}
	cursor.PhaseInventory = fingerprint
	// The fingerprint covers the full sorted listing before consumed directory
	// entries are released. Only unapplied entries are needed by this slice.
	clear(entries[:cursor.Offset])
	complete, err = s.applyRecoveryPhase(ctx, &cursor, deadline, len(entries), func(i int) error {
		entry := entries[i]
		entries[i] = nil
		return s.recoverRequestedMiss(ctx, entry, inventory, candidateInventory)
	})
	if err != nil || !complete {
		return false, err
	}
	if err := s.indexStep("recovery-completing"); err != nil {
		return false, err
	}
	if err := s.completeSessionIndexRecovery(ctx, cursor.Generation, revision); err != nil {
		return false, err
	}
	if err := s.indexStep("recovery-complete"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) prepareRecoveryCursor(ctx context.Context) (sessionRecoveryCursor, bool, error) {
	var marker sessionIndexMarker
	err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if err == nil && marker.Version == 1 && marker.Complete {
		if marker.MembershipFenced {
			if _, err := s.sessionMembershipRevision(); err != nil {
				return sessionRecoveryCursor{}, false, err
			}
		}
		lost, probeErr := s.sessionIndexDirectoriesLost()
		if probeErr != nil {
			return sessionRecoveryCursor{}, false, probeErr
		}
		if !lost {
			return sessionRecoveryCursor{}, true, nil
		}
		// Recreated empty indexes require the existing scheduled census phases.
		err = ErrSessionIndexRecoveryRequired
	}
	var cursor sessionRecoveryCursor
	cursorErr := readRecoveryJSON(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor)
	validCursor := cursorErr == nil && cursor.validChecksum() && cursor.Version == 1 && cursor.Generation != "" && cursor.Phase >= 0 && cursor.Phase <= 2 && cursor.Offset >= 0
	if err != nil || marker.Version != 1 || marker.Generation == "" || !validCursor || cursor.Generation != marker.Generation {
		// New requests fence the final certificate, but do not change owner
		// application already covered by an equivalent complete inventory.
		// The fresh census below must match both membership and fingerprint
		// before retaining phase zero/one progress; candidate facts also match
		// their phase fingerprint. Requested misses restart for the new generation.
		resume := err == nil && marker.Version == 1 && !marker.Complete && marker.Generation != "" && validCursor
		if err := s.indexStep("recovery-begin"); err != nil {
			return sessionRecoveryCursor{}, false, err
		}
		generation, err := s.beginCollectorSessionIndexRecovery(ctx)
		if err != nil {
			return sessionRecoveryCursor{}, false, err
		}
		if resume {
			cursor.Generation = generation
			if cursor.Phase == 2 {
				cursor.Offset, cursor.PhaseInventory = 0, ""
			}
		} else {
			cursor = sessionRecoveryCursor{Version: 1, Generation: generation}
		}
		if err := s.indexStep("recovery-incomplete"); err != nil {
			return sessionRecoveryCursor{}, false, err
		}
	}
	return cursor, false, nil
}

func (s *Store) saveRecoveryCursor(cursor *sessionRecoveryCursor) error {
	cursor.Checksum = ""
	cursor.Checksum = phaseFingerprint(*cursor)
	return local.Write(filepath.Join(s.home, sessionRecoveryCursorFile), *cursor)
}

func (s *Store) applyRecoveryPhase(ctx context.Context, cursor *sessionRecoveryCursor, deadline time.Time, count int, apply func(int) error) (bool, error) {
	for cursor.Offset < count {
		if !time.Now().Before(deadline) {
			return false, s.saveRecoveryCursor(cursor)
		}
		if err := ctx.Err(); err != nil {
			if checkpointErr := s.saveRecoveryCursor(cursor); checkpointErr != nil {
				return false, errors.Join(err, checkpointErr)
			}
			return false, err
		}
		if err := apply(cursor.Offset); err != nil {
			return false, err
		}
		cursor.Offset++
	}
	return true, nil
}

func (cursor sessionRecoveryCursor) validChecksum() bool {
	checksum := cursor.Checksum
	cursor.Checksum = ""
	return checksum != "" && checksum == phaseFingerprint(cursor)
}
