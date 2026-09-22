package collector

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// ErrNoTranscript means a registration names no transcript file yet, or the
// file it names no longer exists.
var ErrNoTranscript = errors.New("the session's transcript is not available on this machine")

// ReadLocalBundle builds, in memory, the source bundle a collector pass would
// build for reg from its transcript as it is now: the same adapter filter,
// plus the hook evidence already published or pending for the session. It
// takes no lock and writes, registers, and uploads nothing, so it is safe
// beside a running collector and while collection is paused. The transcript
// is append-only and a torn final record is excluded exactly as a collector
// pass excludes it.
func ReadLocalBundle(home string, reg archive.SessionRegistration, capturedAt time.Time) (archive.SourceBundle, error) {
	if reg.TranscriptPath == "" {
		return archive.SourceBundle{}, ErrNoTranscript
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	filtered, _, err := filterTranscript(adapter, reg, DefaultMaxTranscriptBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return archive.SourceBundle{}, ErrNoTranscript
		}
		return archive.SourceBundle{}, fmt.Errorf("filter transcript: %w", err)
	}
	store := OpenLocalStoreReadOnly(home)
	var evidence []archive.SupplementalEvidence
	if published, _, _, found, err := store.LoadPublished(reg.ArchiveSessionID); err == nil && found {
		evidence = published.SupplementalEvidence
	} else if last, _, found, err := store.LoadLastPublished(reg.ArchiveSessionID); err == nil && found {
		evidence = last.SupplementalEvidence
	}
	if req, found, err := store.loadRequest(reg.ArchiveSessionID); err == nil && found {
		evidence = mergeSupplementalEvidence(evidence, req.HookEvidence)
	}
	return archive.NewSourceBundle(reg, adapter, filtered, capturedAt, evidence)
}

// FilterTranscriptFile filters one native transcript file that has no
// registration, for a harness named by the caller. startedAt stands in for
// the fresh-start proof a Cursor text transcript otherwise needs. Nothing is
// registered, written, or uploaded.
func FilterTranscriptFile(harness, path string, startedAt time.Time) (archive.FilteredTranscript, archive.Adapter, error) {
	adapter, err := archive.NewAdapter(harness)
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: adapter.Name()}, TranscriptPath: path, SessionStartedAt: startedAt}
	filtered, _, err := filterTranscript(adapter, reg, DefaultMaxTranscriptBytes)
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	return filtered, adapter, nil
}
