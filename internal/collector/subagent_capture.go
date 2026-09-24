package collector

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func materializeSubagentCandidates(local *LocalStore, opts Options) map[string]error {
	issues := map[string]error{}
	candidates, err := local.LoadSubagentCandidates()
	if err != nil {
		issues["subagent-candidates"] = err
		return issues
	}
	for _, candidate := range candidates {
		if err := materializeSubagentCandidate(local, candidate, opts); err != nil {
			issues[candidate.ArchiveSessionID] = err
		}
	}
	return issues
}

func materializeSubagentCandidate(local *LocalStore, candidate SubagentCandidate, opts Options) error {
	parent, found, err := local.LoadRegistration(candidate.ParentArchiveSessionID)
	if err != nil {
		return err
	}
	if !found || parent.NativeSessionID != candidate.ParentNativeSessionID || parent.ProjectID != candidate.ProjectID || parent.ProjectRoot != candidate.ProjectRoot || !strings.EqualFold(parent.Harness.Name, candidate.Harness.Name) || (opts.AcceptSession != nil && !opts.AcceptSession(parent)) {
		return rejectSubagentCandidate(local, candidate, "subagent_parent_ownership_unavailable")
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: candidate.ArchiveSessionID, NativeSessionID: candidate.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness,
		TranscriptPath: candidate.TranscriptPath, RegisteredAt: candidate.ObservedAt,
		ParentSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		SubagentID: candidate.AgentID, SubagentObservedAt: candidate.ObservedAt,
		// The child is admitted with its parent, into the same destination,
		// and by the same import when the parent was imported.
		AdmittedAt: parent.AdmittedAt, Origin: parent.Origin, ImportBatch: parent.ImportBatch,
	}
	if parent.Imported() {
		// Its start is set below from the earliest native record.
		reg.StartedAtSource = archive.StartedAtSourceTranscript
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return rejectSubagentCandidate(local, candidate, "subagent_format_unavailable")
	}
	filtered, _, err := filterTranscript(adapter, reg, opts.maxTranscriptBytes())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("subagent transcript is not available yet: %w", err)
		}
		return rejectSubagentCandidate(local, candidate, "subagent_transcript_unavailable")
	}
	if filtered.NativeStartAt.IsZero() && len(filtered.Records) == 0 {
		return errors.New("subagent transcript is empty; waiting for native records")
	}
	reg.SessionStartedAt = filtered.NativeStartAt
	if err := validateSubagentTranscript(reg, filtered); err != nil {
		return rejectSubagentCandidate(local, candidate, "subagent_provenance_unavailable")
	}
	if reg.SessionStartedAt.Before(parent.SessionStartedAt) || reg.SessionStartedAt.After(candidate.ObservedAt) || (opts.AcceptSession != nil && !opts.AcceptSession(reg)) {
		return rejectSubagentCandidate(local, candidate, "subagent_start_ineligible")
	}
	if existing, found, err := local.LoadRegistration(reg.ArchiveSessionID); err != nil {
		return err
	} else if found && (existing.ParentSessionID != reg.ParentSessionID || existing.ParentNativeSessionID != reg.ParentNativeSessionID || existing.ProjectID != reg.ProjectID || existing.ProjectRoot != reg.ProjectRoot || !strings.EqualFold(existing.Harness.Name, reg.Harness.Name) || existing.SubagentID != reg.SubagentID || existing.TranscriptPath != reg.TranscriptPath || !existing.SessionStartedAt.Equal(reg.SessionStartedAt)) {
		return rejectSubagentCandidate(local, candidate, "subagent_registration_conflict")
	}
	if err := local.SaveRegistration(reg); err != nil {
		return err
	}
	if candidate.Origin == archive.SessionOriginImport {
		// No SubagentStop fired for a subagent backfill found.
		return local.acknowledgeSubagentCandidate(candidate)
	}
	lifecycle, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindLifecycleHook, ObservedAt: candidate.ObservedAt,
		Provenance: "hook:" + strings.ToLower(reg.Harness.Name) + ":subagentstop",
		Payload:    map[string]any{"event_name": "SubagentStop", "agent_id": candidate.AgentID},
	}})
	if err != nil {
		return err
	}
	if len(lifecycle) > 0 {
		if err := local.SaveRequest(reg.ArchiveSessionID, "subagentstop", candidate.ObservedAt, lifecycle[0]); err != nil {
			return err
		}
	}
	return local.acknowledgeSubagentCandidate(candidate)
}

// validateSubagentTranscript is called before registration and on every later
// scan because the hook-provided path is mutable local state.
func validateSubagentTranscript(reg archive.SessionRegistration, filtered archive.FilteredTranscript) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	if !filtered.NativeStartComplete || filtered.NativeStartAt.IsZero() || filtered.NativeEndAt.IsZero() || reg.SubagentObservedAt.IsZero() || filtered.NativeEndAt.After(reg.SubagentObservedAt) {
		return errors.New("subagent transcript has incomplete native timestamp provenance")
	}
	if !filtered.NativeStartAt.Equal(reg.SessionStartedAt) {
		return errors.New("subagent native start changed after registration")
	}
	if len(filtered.AgentIDs) == 0 {
		return errors.New("subagent transcript has no child agent identity")
	}
	if len(filtered.SessionIDs) == 0 {
		return errors.New("subagent transcript has no parent session identity")
	}
	for _, id := range filtered.SessionIDs {
		if id != reg.ParentNativeSessionID {
			return errors.New("subagent transcript parent session identity mismatch")
		}
	}
	for _, id := range filtered.AgentIDs {
		if id != reg.SubagentID {
			return errors.New("subagent transcript agent identity mismatch")
		}
	}
	return nil
}

func rejectSubagentCandidate(local *LocalStore, candidate SubagentCandidate, code string) error {
	evidence, err := archive.NewLinkedSessionEvidence(candidate.ArchiveSessionID, archive.LinkedSessionUnavailable, candidate.ObservedAt)
	if err != nil {
		return err
	}
	// A parent retention has forgotten has nobody left to notify. The
	// candidate is still acknowledged: retrying it would report the same
	// permanent condition on every pass.
	if err := local.SaveRequest(candidate.ParentArchiveSessionID, code, candidate.ObservedAt, evidence); err != nil && !errors.Is(err, ErrSessionNotRegistered) {
		return err
	}
	if err := local.acknowledgeSubagentCandidate(candidate); err != nil {
		return err
	}
	return fmt.Errorf("%s", code)
}

// Retry link notification from durable publication state even when the child
// has no new content or its live transcript has gone away. The notification
// stays urgent so a repaired link reaches the parent on the next pass rather
// than waiting out the upload debounce; SaveRequest drops evidence the pending
// request already carries, so retrying a parent that never publishes is a
// no-op instead of an unbounded append.
func markPublishedSubagent(local *LocalStore, reg archive.SessionRegistration) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	_, publishedAt, published, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !published {
		return err
	}
	if _, found, err := local.LoadRegistration(reg.ParentSessionID); err != nil || !found {
		return err
	}
	parent, _, parentStatus, found, err := local.LoadPublished(reg.ParentSessionID)
	if err != nil {
		return err
	}
	if found && parentStatus == CacheStatusBlocked {
		// A blocked parent cannot republish, and blocking acknowledges its
		// request, so a notification written now would be written and
		// discarded again on every pass for as long as the gap lasts. The link
		// is announced once the parent is unblocked and its cached bundle
		// still lacks it.
		return nil
	}
	if found {
		for _, link := range parent.LinkedSessions {
			if link.SessionID == reg.ArchiveSessionID && link.Status == archive.LinkedSessionPublished {
				return nil
			}
		}
	}
	evidence, err := archive.NewLinkedSessionEvidence(reg.ArchiveSessionID, archive.LinkedSessionPublished, publishedAt)
	if err != nil {
		return err
	}
	return local.SaveRequest(reg.ParentSessionID, "subagent-published", publishedAt, evidence)
}
