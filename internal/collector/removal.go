package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// RemovalReason says why this machine forgot a session it had archived.
type RemovalReason string

// Removal reasons. Backfill reports a session with a removal record as
// removed_by_retention or removed_by_undo and skips it unless
// --include-removed is set.
const (
	// RemovalReasonRetention means retention forgot the session.
	RemovalReasonRetention RemovalReason = "retention"
	// RemovalReasonUndo means `agent-archive backfill undo` removed it.
	RemovalReasonUndo RemovalReason = "undo"
)

// RemovalRecord outlives a forgotten session. ForgetSession removes the
// native-session index entry, so without it backfill would import the
// session again on its next run. It holds no native ID, path, or content.
type RemovalRecord struct {
	Harness string        `json:"app"`
	Reason  RemovalReason `json:"reason"`
	At      time.Time     `json:"at"`
}

// removalPath names a record by the hash of the app and native ID, like the
// native-session index: a native ID is harness-controlled input and never a
// file name. The app is part of the key because two apps' IDs can collide.
// It is canonicalised first, so a registration written under "claude-code"
// (a hand-edited hook) finds the record backfill looks up under "claude".
func removalPath(home, harness, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(removalHarness(harness) + "\x00" + nativeSessionID))
	return filepath.Join(home, "forgotten", hex.EncodeToString(sum[:])+".json")
}

// removalHarness is the app name removal records are keyed by, as the CLI
// canonicalises hook harness names.
func removalHarness(harness string) string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness == "claude-code" {
		return "claude"
	}
	return harness
}

// RecordRemoval writes forgotten/<sha256(harness + "\x00" + nativeSessionID)>.json.
// A later removal of the same session replaces the earlier record.
func (s *LocalStore) RecordRemoval(harness, nativeSessionID string, reason RemovalReason, at time.Time) error {
	if strings.TrimSpace(harness) == "" || strings.TrimSpace(nativeSessionID) == "" {
		return errors.New("app and native session ID are required")
	}
	if reason != RemovalReasonRetention && reason != RemovalReasonUndo {
		return fmt.Errorf("unknown removal reason %q", reason)
	}
	if err := local.Write(removalPath(s.home, harness, nativeSessionID), RemovalRecord{Harness: harness, Reason: reason, At: at.UTC()}); err != nil {
		return fmt.Errorf("record removal: %w", err)
	}
	return nil
}

// Removal reports the record for a native session, if any.
func (s *LocalStore) Removal(harness, nativeSessionID string) (RemovalRecord, bool, error) {
	if strings.TrimSpace(harness) == "" || strings.TrimSpace(nativeSessionID) == "" {
		return RemovalRecord{}, false, errors.New("app and native session ID are required")
	}
	var record RemovalRecord
	err := local.Read(removalPath(s.home, harness, nativeSessionID), &record)
	if errors.Is(err, os.ErrNotExist) {
		return RemovalRecord{}, false, nil
	}
	if err != nil {
		return RemovalRecord{}, false, fmt.Errorf("read removal record: %w", err)
	}
	return record, true, nil
}
