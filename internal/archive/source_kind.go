package archive

import (
	"errors"
	"strings"
)

// SourceKind says where the collector reads a session's evidence from. It is
// fixed when the session is registered: a session never switches sources, so
// a resumed chat cannot change formats under nativeEvidenceExtends.
type SourceKind string

const (
	// SourceKindFile is a transcript file at SessionRegistration.TranscriptPath.
	// It is the zero value, so every registration written before source kinds
	// existed is a file session.
	SourceKindFile SourceKind = ""
	// SourceKindCursorSQLite is one chat in Cursor's state.vscdb, named by
	// SessionRegistration.SourceKey (its composer ID). Such a session has no
	// transcript path and never adopts one a hook reports.
	SourceKindCursorSQLite SourceKind = "cursor-sqlite"
)

// ReadsTranscriptFile reports whether the session is read from a transcript
// file, and so may adopt a transcript path a hook reports.
func (r SessionRegistration) ReadsTranscriptFile() bool {
	return r.SourceKind == SourceKindFile
}

func (r SessionRegistration) validateSource() error {
	switch r.SourceKind {
	case SourceKindFile:
		return nil
	case SourceKindCursorSQLite:
		if strings.TrimSpace(r.SourceKey) == "" {
			return errors.New("a Cursor database session requires its composer ID as the source key")
		}
		// The composer ID is the chat's native session ID, which is what
		// hooks and backfill deduplicate on.
		if r.SourceKey != r.NativeSessionID {
			return errors.New("a Cursor database session's source key must be its native session ID")
		}
		if !strings.EqualFold(strings.TrimSpace(r.Harness.Name), "cursor") {
			return errors.New("a Cursor database session must be a Cursor session")
		}
		if r.TranscriptPath != "" {
			return errors.New("a Cursor database session has no transcript path")
		}
		return nil
	default:
		return errors.New("unknown session source kind")
	}
}
