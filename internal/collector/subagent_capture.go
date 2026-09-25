package collector

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// materializeSubagentCandidates registers each candidate a hook left, or
// rejects it, and returns the per-candidate failures keyed by the
// candidate's archive session ID. One unreadable candidate fails only
// itself.
func materializeSubagentCandidates(local *state.Store, opts Options) map[string]error {
	candidates, issues, err := local.ScanSubagentCandidates()
	if err != nil {
		return map[string]error{"subagent-candidates": err}
	}
	for _, candidate := range candidates {
		if err := materializeSubagentCandidate(local, candidate, opts); err != nil {
			issues[candidate.ArchiveSessionID] = err
		}
	}
	return issues
}

func materializeSubagentCandidate(local *state.Store, candidate state.SubagentCandidate, opts Options) error {
	parent, found, err := local.LoadRegistration(candidate.ParentArchiveSessionID)
	if err != nil {
		return err
	}
	if !found || parent.NativeSessionID != candidate.ParentNativeSessionID || parent.ProjectID != candidate.ProjectID || parent.ProjectRoot != candidate.ProjectRoot || !strings.EqualFold(parent.Harness.Name, candidate.Harness.Name) || (opts.AcceptSession != nil && !opts.AcceptSession(parent)) {
		return rejectSubagentCandidate(local, candidate, "subagent_parent_ownership_unavailable")
	}
	var startedAtSource archive.StartedAtSource
	if parent.Imported() {
		// Its start is set below from the earliest native record.
		startedAtSource = archive.StartedAtSourceTranscript
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
		DestinationID:   parent.DestinationID,
		StartedAtSource: startedAtSource,
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
	if subagentTranscriptEmpty(filtered) {
		if candidate.Origin == archive.SessionOriginImport {
			// A transcript backfill found is history: it will not grow.
			return rejectSubagentCandidate(local, candidate, "subagent_transcript_unavailable")
		}
		return errors.New("subagent transcript is empty; waiting for native records")
	}
	reg.SessionStartedAt = filtered.NativeStartAt
	if code := checkNewSubagent(filtered, reg.ParentNativeSessionID, reg.SubagentID, parent.SessionStartedAt, candidate.ObservedAt); code != "" {
		return rejectSubagentCandidate(local, candidate, code)
	}
	if opts.AcceptSession != nil && !opts.AcceptSession(reg) {
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
		return local.AcknowledgeSubagentCandidate(candidate)
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
	return local.AcknowledgeSubagentCandidate(candidate)
}

// subagentTranscriptEmpty reports a subagent transcript with no native
// records yet.
func subagentTranscriptEmpty(filtered archive.FilteredTranscript) bool {
	return filtered.NativeStartAt.IsZero() && len(filtered.Records) == 0
}

// checkNewSubagent applies the checks a subagent transcript with records must
// pass before it is first registered, and returns the code it is rejected
// with, or "".
func checkNewSubagent(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, parentStartedAt, observedAt time.Time) string {
	if checkSubagentProvenance(filtered, parentNativeSessionID, agentID, filtered.NativeStartAt, observedAt) != nil {
		return "subagent_provenance_unavailable"
	}
	if filtered.NativeStartAt.Before(parentStartedAt) || filtered.NativeStartAt.After(observedAt) {
		return "subagent_start_ineligible"
	}
	return ""
}

// CheckImportedSubagent reports why the collector would refuse to register a
// subagent transcript backfill found, observed at observedAt, or nil when it
// would register it: the transcript is empty, its native timestamps are
// incomplete or end after observedAt, it names another parent session or
// agent, or it starts before its parent. Backfill's plan runs it so the
// subagents it counts are the ones the collector registers.
func CheckImportedSubagent(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, parentStartedAt, observedAt time.Time) error {
	if subagentTranscriptEmpty(filtered) {
		return errors.New("subagent transcript is empty")
	}
	if code := checkNewSubagent(filtered, parentNativeSessionID, agentID, parentStartedAt, observedAt); code != "" {
		return errors.New(code)
	}
	return nil
}

// validateSubagentTranscript is called before registration and on every later
// scan because the hook-provided path is mutable local state.
func validateSubagentTranscript(reg archive.SessionRegistration, filtered archive.FilteredTranscript) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	return checkSubagentProvenance(filtered, reg.ParentNativeSessionID, reg.SubagentID, reg.SessionStartedAt, reg.SubagentObservedAt)
}

// checkSubagentProvenance checks that a subagent transcript's native
// timestamps are complete, start at startedAt, and end no later than
// observedAt, and that its records name only the parent session and the
// agent.
func checkSubagentProvenance(filtered archive.FilteredTranscript, parentNativeSessionID, agentID string, startedAt, observedAt time.Time) error {
	if !filtered.NativeStartComplete || filtered.NativeStartAt.IsZero() || filtered.NativeEndAt.IsZero() || observedAt.IsZero() || filtered.NativeEndAt.After(observedAt) {
		return errors.New("subagent transcript has incomplete native timestamp provenance")
	}
	if !filtered.NativeStartAt.Equal(startedAt) {
		return errors.New("subagent native start changed after registration")
	}
	if len(filtered.AgentIDs) == 0 {
		return errors.New("subagent transcript has no child agent identity")
	}
	if len(filtered.SessionIDs) == 0 {
		return errors.New("subagent transcript has no parent session identity")
	}
	for _, id := range filtered.SessionIDs {
		if id != parentNativeSessionID {
			return errors.New("subagent transcript parent session identity mismatch")
		}
	}
	for _, id := range filtered.AgentIDs {
		if id != agentID {
			return errors.New("subagent transcript agent identity mismatch")
		}
	}
	return nil
}

func rejectSubagentCandidate(local *state.Store, candidate state.SubagentCandidate, code string) error {
	evidence, err := archive.NewLinkedSessionEvidence(candidate.ArchiveSessionID, archive.LinkedSessionUnavailable, candidate.ObservedAt)
	if err != nil {
		return err
	}
	// A parent retention has forgotten has nobody left to notify. The
	// candidate is still acknowledged: retrying it would report the same
	// permanent condition on every pass.
	if err := local.SaveRequest(candidate.ParentArchiveSessionID, code, candidate.ObservedAt, evidence); err != nil && !errors.Is(err, state.ErrSessionNotRegistered) {
		return err
	}
	if err := local.AcknowledgeSubagentCandidate(candidate); err != nil {
		return err
	}
	return fmt.Errorf("%s", code)
}

// announceSubagent tells a published subagent's parent that it is published,
// from the subagent's durable publication state, even when it has no new
// content or its live transcript has gone away. The notification stays
// urgent so a repaired link reaches the parent on the next pass rather than
// waiting out the upload debounce; SaveRequest drops evidence the pending
// request already carries, so retrying a parent that never publishes is a
// no-op instead of an unbounded append. The parent is read only as far as
// its summary: decoding its whole bundle once for each of its subagents, on
// every pass, was most of the cost of a pass.
func announceSubagent(local *state.Store, reg archive.SessionRegistration, child *state.Published) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	_, publishedAt, published := child.LastPublished()
	if !published {
		return nil
	}
	if _, found, err := local.LoadRegistration(reg.ParentSessionID); err != nil || !found {
		return err
	}
	parent, found, err := local.LoadPublishedSummary(reg.ParentSessionID)
	if err != nil {
		return err
	}
	if found && parent.Status == state.CacheStatusBlocked {
		// A blocked parent cannot republish, and blocking acknowledges its
		// request, so a notification written now would be written and
		// discarded again on every pass for as long as the gap lasts. The link
		// is announced once the parent is unblocked and its cached bundle
		// still lacks it.
		return nil
	}
	if found && parent.LinksPublished(reg.ArchiveSessionID) {
		return nil
	}
	evidence, err := archive.NewLinkedSessionEvidence(reg.ArchiveSessionID, archive.LinkedSessionPublished, publishedAt)
	if err != nil {
		return err
	}
	return local.SaveRequest(reg.ParentSessionID, "subagent-published", publishedAt, evidence)
}
