package capture

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"

	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func handleSubagentStop(store *state.Store, cfg config.Config, event agentapi.LifecycleEvent, now time.Time, after func(effectName) error) error {
	harness, parentNativeID := string(event.Session.Agent), event.Session.NativeID

	parentID, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(harness)), NativeID: parentNativeID})
	if err != nil {
		return fmt.Errorf("look up parent archive session ID: %w", err)
	}
	if !found {
		return nil
	}
	parent, found, err := store.LoadRegistration(parentID)
	if err != nil {
		return err
	}
	if !found || archive.CanonicalHarness(parent.Harness.Name) != archive.CanonicalHarness(harness) || !cfg.AcceptSession(parent) {
		return nil
	}

	if _, err := store.UpdateRegistration(parentID, func(reg *archive.SessionRegistration) error {
		reg.HookObservedAt = now
		return nil
	}); err != nil {
		return err
	}

	agentID := event.Child.ID
	if agentID == "" {
		return saveSubagentCaptureGap(store, parent.ArchiveSessionID, "subagent_identity_unavailable", event.Child.MissingDetail, now)
	}
	childNativeID := parent.NativeSessionID + ":subagent:" + agentID
	childKey := agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(parent.Harness.Name)), NativeID: childNativeID}
	childID, _, err := store.EnsureArchiveSessionID(childKey)
	if err != nil {
		if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			err = errors.Join(err, store.RequestSessionIndexRecovery(childKey))
		}
		return fmt.Errorf("assign subagent archive session ID: %w", err)
	}
	if err := effectBoundary(after, effectChildReservation); err != nil {
		return err
	}
	status := archive.LinkedSessionUnavailable
	path := event.Child.Path
	if event.Child.CaptureTranscript && path != "" {
		status = archive.LinkedSessionPending
	}
	if existing, childFound, err := store.LoadRegistration(childID); err != nil {
		return err
	} else if childFound {
		if existing.ParentSessionID != parent.ArchiveSessionID || existing.ParentNativeSessionID != parent.NativeSessionID || existing.ProjectID != parent.ProjectID || existing.ProjectRoot != parent.ProjectRoot || archive.CanonicalHarness(existing.Harness.Name) != archive.CanonicalHarness(parent.Harness.Name) || existing.SubagentID != agentID {
			return nil
		}
		if _, _, published, err := store.LoadLastPublished(childID); err != nil {
			return err
		} else if published {
			status = archive.LinkedSessionPublished
		}
	}
	if err := saveLinkedSessionEvidence(store, parent.ArchiveSessionID, childID, status, now); err != nil {
		return err
	}
	if err := effectBoundary(after, effectChildLink); err != nil {
		return err
	}
	if !event.Child.CaptureTranscript || path == "" {
		return nil
	}
	err = store.SaveSubagentCandidate(state.SubagentCandidate{
		ArchiveSessionID: childID, NativeSessionID: childNativeID,
		ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness,
		AgentID: agentID, TranscriptPath: path, ObservedAt: now,
		AgentType: event.Child.Type,
	})
	if err != nil {
		return err
	}
	return effectBoundary(after, effectChildCandidate)
}

func saveLinkedSessionEvidence(store *state.Store, parentID, childID string, status archive.LinkedSessionStatus, observedAt time.Time) error {
	evidence, err := archive.NewLinkedSessionEvidence(childID, status, observedAt)
	if err != nil {
		return fmt.Errorf("filter subagent link: %w", err)
	}
	return store.SaveRequest(parentID, "subagent-link", observedAt, evidence)
}

func saveSubagentCaptureGap(store *state.Store, parentID, code, detail string, observedAt time.Time) error {
	evidence, err := archive.NewCaptureGapEvidence(code, detail, "hook:subagent-link", observedAt)
	if err != nil {
		return err
	}
	return store.SaveRequest(parentID, "subagent-link-unavailable", observedAt, evidence)
}
