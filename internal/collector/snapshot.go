package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
)

// ErrNoTranscript means a registration names no transcript file yet, or the
// file it names no longer exists, or, for a Cursor database chat, the chat
// or the database is gone.
var ErrNoTranscript = errors.New("the session's transcript is not available on this machine")

// ReadLocalBundle builds, in memory, the source bundle a collector pass would
// build for reg from its source as it is now: the same adapter filter,
// plus the hook evidence already published or pending for the session. It
// takes no lock and writes, registers, and uploads nothing, so it is safe
// beside a running collector and while collection is paused. The transcript
// is append-only and a torn final record is excluded exactly as a collector
// pass excludes it. A Cursor database chat is read from cursorDatabase
// (empty means the one under the user's home), through a snapshot of its
// own when Cursor is running, which is removed before it returns.
func ReadLocalBundle(ctx context.Context, home string, reg archive.SessionRegistration, capturedAt time.Time, cursorDatabase string) (archive.SourceBundle, error) {
	source, ok := newSourceReader(reg, Options{CursorDatabase: cursorDatabase})
	if !ok {
		return archive.SourceBundle{}, ErrNoTranscript
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	filtered, _, err := source.Filter(ctx, adapter, DefaultMaxTranscriptBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return archive.SourceBundle{}, ErrNoTranscript
		}
		return archive.SourceBundle{}, fmt.Errorf("filter transcript: %w", err)
	}
	store := state.OpenReadOnly(home)
	var evidence []archive.SupplementalEvidence
	if published, err := store.LoadPublishedState(reg.ArchiveSessionID); err == nil {
		if cached, _, _, found := published.Cached(); found {
			evidence = cached.SupplementalEvidence
		}
	}
	if req, found, err := store.LoadRequest(reg.ArchiveSessionID); err == nil && found {
		evidence = mergeSupplementalEvidence(evidence, req.HookEvidence)
	}
	return archive.NewSourceBundle(reg, adapter, filtered, capturedAt, evidence)
}

// LastActivity is when reg's source last changed: its transcript's
// modification time, or a Cursor database chat's lastUpdatedAt. ok is false
// when that can't be read.
func LastActivity(ctx context.Context, reg archive.SessionRegistration, cursorDatabase string) (time.Time, bool) {
	if reg.SourceKind == archive.SourceKindCursorSQLite {
		sig, err := cursorstore.ReadSignature(ctx, Options{CursorDatabase: cursorDatabase}.cursorDatabase(), reg.SourceKey)
		if err != nil || sig.LastUpdatedAt <= 0 {
			return time.Time{}, false
		}
		return time.UnixMilli(sig.LastUpdatedAt), true
	}
	if reg.TranscriptPath == "" {
		return time.Time{}, false
	}
	info, err := os.Stat(reg.TranscriptPath)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// FilterTranscriptFile filters one native transcript file that has no
// registration, for a harness named by the caller. startedAt stands in for
// the fresh-start proof a Cursor text transcript otherwise needs. A
// transcript over any of the collector's size limits is an error wrapping
// archive.ErrRecordTooLarge, as for FilterCursorChat. Nothing is registered,
// written, or uploaded.
func FilterTranscriptFile(harness, path string, startedAt time.Time) (archive.FilteredTranscript, archive.Adapter, error) {
	adapter, err := archive.NewAdapter(harness)
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: adapter.Name()}, TranscriptPath: path, SessionStartedAt: startedAt}
	filtered, _, err := filterTranscript(adapter, reg, DefaultMaxTranscriptBytes)
	if errors.Is(err, errRecordTooLarge) || errors.Is(err, errTranscriptTooLarge) {
		return archive.FilteredTranscript{}, nil, fmt.Errorf("%w: %w", archive.ErrRecordTooLarge, err)
	}
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	return filtered, adapter, nil
}
