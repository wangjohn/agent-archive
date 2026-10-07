package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const generationHeadsDir = "generation-heads"

const generationNodesDir = "generation-nodes"

const generationRecoveryDir = "generation-recovery"

// A head is deliberately small: hooks never decode retained source or the
// complete generation chain. Immutable nodes and receipts survive content expiry.
type generationHead struct {
	Version    int                  `json:"version"`
	Key        agentmeta.SessionKey `json:"key"`
	Active     string               `json:"active"`
	Retired    bool                 `json:"retired,omitempty"`
	Transition string               `json:"transition,omitempty"`
}

type generationNode struct {
	Version      int                  `json:"version"`
	Key          agentmeta.SessionKey `json:"key"`
	ID           string               `json:"id"`
	Previous     string               `json:"previous,omitempty"`
	PreviousRoot string               `json:"previous_root,omitempty"`
}

type generationRecovery struct {
	PriorMetadataSHA256  string                       `json:"prior_metadata_sha256"`
	PriorSourceSetSHA256 string                       `json:"prior_source_set_sha256"`
	Version              int                          `json:"version"`
	Key                  agentmeta.SessionKey         `json:"key"`
	Previous             string                       `json:"previous"`
	Next                 string                       `json:"next"`
	Complete             bool                         `json:"complete,omitempty"`
	Registration         *archive.SessionRegistration `json:"registration,omitempty"`
	Pending              *PendingPublication          `json:"pending,omitempty"`
	Request              *Request                     `json:"request,omitempty"`
}

func (s *Store) generationHeadPath(key agentmeta.SessionKey) string {
	return filepath.Join(s.home, generationHeadsDir, filepath.Base(qualifiedSessionIndexPath(s.home, key)))
}

func (s *Store) generationNodePath(id string) string {
	return filepath.Join(s.home, generationNodesDir, id+".json")
}

func (s *Store) generationRecoveryPath(id string) string {
	return filepath.Join(s.home, generationRecoveryDir, id+".json")
}

func (s *Store) loadGenerationHead(key agentmeta.SessionKey) (generationHead, bool, error) {
	var h generationHead
	file, err := os.Open(s.generationHeadPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, false, ErrSessionIndexRecoveryRequired
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return h, false, ErrSessionIndexRecoveryRequired
	}
	err = json.Unmarshal(data, &h)
	if err != nil || h.Version != 1 || h.Key != key || !safeFileComponent(h.Active) || h.Transition != "" && !safeFileComponent(h.Transition) || h.Retired && h.Transition != "" {
		return h, false, ErrSessionIndexRecoveryRequired
	}
	return h, true, nil
}

// GenerationSuccessor returns a recorded recovery receipt, including when its
// content has expired. Repeating confirmation must never create another ID.
func (s *Store) GenerationSuccessor(id string) (string, bool, error) {
	if !safeFileComponent(id) {
		return "", false, errors.New("invalid archive session ID")
	}
	r, found, err := readJSON[generationRecovery](s.generationRecoveryPath(id))
	if err != nil || found && (r.Version != 1 || r.Previous != id || !safeFileComponent(r.Next) || r.Next == id || r.Key.Validate() != nil) {
		return "", false, ErrSessionIndexRecoveryRequired
	}
	return r.Next, found, nil
}

// BeginGenerationRecovery commits a confirmed successor under collector.lock.
// Filtering is done before this call. Build receives the latest registration
// under hooks.lock, so lifecycle updates arriving during filtering are retained.
func (s *Store) BeginGenerationRecovery(id string, at time.Time, build func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error)) (string, error) {
	if !safeFileComponent(id) || at.IsZero() {
		return "", errors.New("valid session ID and recovery time required")
	}
	unlock, err := local.NamedLockWait(s.home, "hooks.lock", time.Second)
	if err != nil {
		return "", err
	}
	defer unlock()
	if next, found, err := s.GenerationSuccessor(id); err != nil || found {
		if err != nil {
			return "", err
		}
		if err := s.resumeGenerationRecovery(id); err != nil {
			return "", err
		}
		return next, nil
	}
	old, key, err := s.generationRecoveryOriginal(id)
	if err != nil {
		return "", err
	}
	next, err := local.ID()
	if err != nil {
		return "", err
	}
	reg, pending, err := build(old, next)
	if err != nil {
		return "", err
	}
	if err := validateGenerationSuccessor(old, reg, pending, next, at); err != nil {
		return "", err
	}
	token, err := local.ID()
	if err != nil {
		return "", err
	}
	request := Request{ArchiveSessionID: next, Token: token, Reasons: []string{"generation-recovery"}, RequestedAt: at}
	pending.RequestToken = token
	priorRaw, priorSet, err := s.generationCommittedAuthority(old)
	if err != nil {
		return "", err
	}
	r := generationRecovery{PriorMetadataSHA256: priorRaw, PriorSourceSetSHA256: priorSet, Version: 1, Key: key, Previous: id, Next: next, Registration: &reg, Pending: &pending, Request: &request}
	if err := local.Write(s.generationRecoveryPath(id), r); err != nil {
		return "", err
	}
	if err := s.indexStep("generation-journal"); err != nil {
		return "", err
	}
	return next, s.resumeGenerationRecovery(id)
}

func validateGenerationSuccessor(old, reg archive.SessionRegistration, pending PendingPublication, next string, at time.Time) error {
	if reg.ArchiveSessionID != next || reg.PreviousGenerationID != old.ArchiveSessionID || reg.CaptureFrozen || reg.Validate() != nil || pending.Bundle.ArchiveSessionID != next || pending.Bundle.PreviousGenerationID != old.ArchiveSessionID || !pending.Bundle.Capture.CapturedAt.Equal(at) {
		return errors.New("invalid recovery successor")
	}
	preserved := reg
	preserved.ArchiveSessionID = old.ArchiveSessionID
	preserved.PreviousGenerationID = old.PreviousGenerationID
	preserved.AdmissionStage = old.AdmissionStage
	if reg.AdmissionStage != "" {
		return errors.New("successor cannot inherit predecessor admission stage")
	}
	if !reflect.DeepEqual(preserved, old) {
		return errors.New("recovery cannot alter original admission or provenance")
	}
	return validateGenerationPublication(reg, pending)
}

// Validate the fixed rendered snapshot before committing any routing change.
// A decodable journal must not redirect preserved metadata or carry bytes
// belonging to another generation, even when its registration is intact.
func validateGenerationPublication(reg archive.SessionRegistration, pending PendingPublication) error {
	if err := archive.CheckHistoryMutation(pending.Bundle, archive.Metadata{}); err != nil {
		return err
	}
	return validateGenerationEvidence(reg, pending)
}

// validateGenerationEvidence is shared fixed-source validation beneath recovery fences.
func validateGenerationEvidence(reg archive.SessionRegistration, pending PendingPublication) error {
	bundle := pending.Bundle
	if pending.MetadataOnly || bundle.NativeSessionID != reg.NativeSessionID || bundle.ProjectID != reg.ProjectID || bundle.Capture.Harness.Name != reg.Harness.Name {
		return errors.New("recovery publication identity differs from registration")
	}
	source, err := archive.BuildCompressedSource(bundle)
	if err != nil || source.SHA256 != pending.SourceSHA256 || !bytes.Equal(source.Bytes, pending.SourceBytes) {
		return errors.New("recovery publication source differs from its fixed bundle")
	}
	sourceKey, err := archive.SourceObjectKey(bundle, source.SHA256)
	if err != nil || sourceKey != pending.SourceKey {
		return errors.New("recovery publication source key differs from its fixed bundle")
	}
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil || metadataKey != pending.MetadataKey {
		return errors.New("recovery publication metadata key differs from registration")
	}
	var metadata archive.Metadata
	if json.Unmarshal(pending.MetadataBytes, &metadata) != nil || (metadata.SchemaVersion != archive.MetadataSchemaVersion && metadata.SchemaVersion != archive.HistoryMetadataSchemaVersion) || metadata.ValidateSourceReference() != nil || metadata.SessionID != reg.ArchiveSessionID || metadata.PreviousGenerationID != reg.PreviousGenerationID || metadata.NativeSessionID != reg.NativeSessionID || metadata.ProjectID != reg.ProjectID || metadata.Harness.Name != reg.Harness.Name || !metadata.CapturedAt.Equal(bundle.Capture.CapturedAt) || metadata.SourceBundle != pending.SourceReference() {
		return errors.New("recovery publication metadata differs from its fixed source")
	}
	return nil
}

func (s *Store) generationRecoveryOriginal(id string) (archive.SessionRegistration, agentmeta.SessionKey, error) {
	if err := s.CheckHistoryRecovery(id); err != nil {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, err
	}
	old, found, err := s.LoadRegistration(id)
	if err != nil || !found {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("session is not registered on this machine")
	}
	key, err := registrationKey(old)
	if err != nil {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, err
	}
	active, found, err := s.ArchiveSessionID(key)
	if err != nil || !found || active != id || old.CaptureFrozen {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("recovery requires the active archive generation")
	}
	summary, found, err := s.LoadPublishedSummary(id)
	if err != nil || !found || !summary.Published || summary.BlockedReason != BlockedReasonTranscriptRewritten {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("recovery requires published history blocked by transcript_rewritten")
	}
	if old.AdmissionStage != "" {
		settled, err := s.AdmissionStageReleased(old)
		if err != nil || !settled {
			return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("settle predecessor durable admission cleanup before recovery")
		}
	}
	if pending, err := s.HasPending(id); err != nil || pending {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("settle pending publication with agent-archive sync before recovery")
	}
	if old.ParentSessionID != "" || !old.ReadsTranscriptFile() {
		return archive.SessionRegistration{}, agentmeta.SessionKey{}, errors.New("recovery supports top-level transcript files only")
	}
	return old, key, nil
}

// ResumeGenerationRecoveries replays interrupted local transitions before any
// census, admission replay or storage work. Caller owns collector.lock.
func (s *Store) ResumeGenerationRecoveries(ctx context.Context) error {
	files, err := os.ReadDir(filepath.Join(s.home, generationRecoveryDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}
		id := file.Name()[:len(file.Name())-5]
		r, _, err := readJSON[generationRecovery](s.generationRecoveryPath(id))
		if err != nil {
			return ErrSessionIndexRecoveryRequired
		}
		if r.Version != 1 || r.Previous != id || !safeFileComponent(r.Next) || r.Key.Validate() != nil {
			return ErrSessionIndexRecoveryRequired
		}
		if r.Complete {
			continue
		}
		unlock, err := local.NamedLockWait(s.home, "hooks.lock", time.Second)
		if err != nil {
			return err
		}
		err = s.resumeGenerationRecovery(id)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// resumeGenerationRecovery runs with collector.lock then hooks.lock. Every
// registration/index/request write uses its existing request-lock staging;
// membership renames therefore retain request -> membership lock order.
func (s *Store) resumeGenerationRecovery(id string) error {
	r, found, err := readJSON[generationRecovery](s.generationRecoveryPath(id))
	if err != nil || !found || r.Version != 1 || r.Previous != id || !safeFileComponent(r.Next) || r.Key.Validate() != nil {
		return ErrSessionIndexRecoveryRequired
	}
	if r.Complete {
		return nil
	}
	if r.Registration == nil || r.Pending == nil || r.Request == nil || r.Registration.ArchiveSessionID != r.Next || r.Registration.PreviousGenerationID != id || r.Pending.Bundle.ArchiveSessionID != r.Next || r.Request.ArchiveSessionID != r.Next || r.Request.Token != r.Pending.RequestToken {
		return ErrSessionIndexRecoveryRequired
	}
	// A journal is durable intent, not permission to alter admission. Validate
	// it against the predecessor before even fencing native routing. A retry
	// after freezing differs only in that locally committed capture flag.
	old, present, err := s.LoadRegistration(id)
	if err != nil || !present {
		return ErrSessionIndexRecoveryRequired
	}
	old.CaptureFrozen = false
	key, err := registrationKey(old)
	if err != nil || key != r.Key || validateGenerationSuccessor(old, *r.Registration, *r.Pending, r.Next, r.Request.RequestedAt) != nil {
		return ErrSessionIndexRecoveryRequired
	}
	priorRaw, priorSet, err := s.generationCommittedAuthority(old)
	if err != nil || priorRaw != r.PriorMetadataSHA256 || priorSet != r.PriorSourceSetSHA256 {
		return ErrSessionIndexRecoveryRequired
	}
	if err := s.freezeGenerationRecovery(r); err != nil {
		return fmt.Errorf("freeze recovery predecessor: %w", err)
	}
	if err := s.registerGenerationRecovery(r); err != nil {
		return fmt.Errorf("register recovery successor: %w", err)
	}
	if err := s.activateGenerationRecovery(r); err != nil {
		return fmt.Errorf("activate recovery successor: %w", err)
	}
	return nil
}

func (s *Store) freezeGenerationRecovery(r generationRecovery) error {
	h, present, err := s.loadGenerationHead(r.Key)
	if err != nil {
		return err
	}
	if present && (h.Retired || h.Active != r.Previous && h.Active != r.Next || h.Transition != "" && h.Transition != r.Previous) {
		return ErrSessionIdentityConflict
	}
	h = generationHead{Version: 1, Key: r.Key, Active: r.Previous, Transition: r.Previous}
	if err := local.Write(s.generationHeadPath(r.Key), h); err != nil {
		return err
	}
	if err := s.indexStep("generation-fenced"); err != nil {
		return err
	}
	old, found, err := s.LoadRegistration(r.Previous)
	if err != nil || !found {
		return ErrSessionIndexRecoveryRequired
	}
	actual, err := registrationKey(old)
	if err != nil || actual != r.Key || old.ArchiveSessionID != r.Previous {
		return ErrSessionIdentityConflict
	}
	oldNode := generationNode{Version: 1, Key: r.Key, ID: r.Previous, Previous: old.PreviousGenerationID}
	if prior, found, err := readJSON[generationNode](s.generationNodePath(r.Previous)); err != nil {
		return ErrSessionIndexRecoveryRequired
	} else if found {
		if prior.Key != r.Key || prior.ID != r.Previous || prior.Previous != old.PreviousGenerationID {
			return ErrSessionIdentityConflict
		}
		oldNode = prior
	}
	if err := s.saveGenerationNode(oldNode); err != nil {
		return err
	}
	if err := s.saveGenerationNode(generationNode{Version: 1, Key: r.Key, ID: r.Next, Previous: r.Previous}); err != nil {
		return err
	}
	if err := s.indexStep("generation-nodes"); err != nil {
		return err
	}
	if _, err := s.UpdateRegistration(r.Previous, func(reg *archive.SessionRegistration) error { reg.CaptureFrozen = true; return nil }); err != nil {
		return err
	}
	if err := s.indexStep("generation-frozen"); err != nil {
		return err
	}
	return nil
}

func (s *Store) registerGenerationRecovery(r generationRecovery) error {
	reg := *r.Registration
	if err := s.writeUnderRequestLock(r.Next, s.registrationPath(r.Next), nil, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var prior archive.SessionRegistration
			if json.Unmarshal(current.data, &prior) != nil || !reflect.DeepEqual(prior, reg) {
				return nil, false, ErrSessionIdentityConflict
			}
			return nil, false, nil
		}
		return reg, true, nil
	}); err != nil {
		return err
	}
	if err := s.indexStep("generation-registration"); err != nil {
		return err
	}
	if err := s.SavePending(r.Next, *r.Pending); err != nil {
		return err
	}
	if err := s.indexStep("generation-pending"); err != nil {
		return err
	}
	if err := s.writeUnderRequestLock(r.Next, s.requestPath(r.Next), nil, func(current fileSnapshot) (any, bool, error) {
		if !current.found {
			return *r.Request, true, nil
		}
		// User feedback names an archive ID directly and can arrive while an
		// interrupted transition gates native hooks. Preserve that newer token
		// and evidence rather than overwriting it with the journal's request.
		return mergeRequest(r.Next, current, "generation-recovery", r.Request.RequestedAt, false, nil)
	}); err != nil {
		return err
	}
	if err := s.indexStep("generation-request"); err != nil {
		return err
	}
	return nil
}

func (s *Store) activateGenerationRecovery(r generationRecovery) error {
	if err := s.writeGenerationIndexUnderRequestLock(r.Next, qualifiedSessionIndexPath(s.home, r.Key), nil, func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var prior qualifiedSessionIndexEntry
			if json.Unmarshal(current.data, &prior) != nil || prior.Version != 1 || prior.Agent != r.Key.Agent || prior.NativeID != r.Key.NativeID {
				return nil, false, ErrSessionIdentityConflict
			}
			// A gated hook requests derived-index recovery before queuing its
			// admission intent. The journal remains authoritative over this
			// exact content-free marker; arbitrary owners still fail closed.
			requested := prior == (qualifiedSessionIndexEntry{Version: 1, Agent: r.Key.Agent, NativeID: r.Key.NativeID, Recovery: true})
			if !requested && (prior.Conflict || prior.Absent || prior.Recovery || prior.Reservation != "" || prior.ArchiveSessionID != r.Previous && prior.ArchiveSessionID != r.Next) {
				return nil, false, ErrSessionIdentityConflict
			}
		}
		return indexEntry(r.Key, r.Next), true, nil
	}); err != nil {
		return err
	}
	if err := s.indexStep("generation-index"); err != nil {
		return err
	}
	h := generationHead{Version: 1, Key: r.Key, Active: r.Next}
	if err := local.Write(s.generationHeadPath(r.Key), h); err != nil {
		return err
	}
	if err := s.indexStep("generation-active"); err != nil {
		return err
	}
	r.Complete = true
	r.Registration = nil
	r.Pending = nil
	r.Request = nil
	if err := local.Write(s.generationRecoveryPath(r.Previous), r); err != nil {
		return err
	}
	return s.indexStep("generation-complete")
}

func (s *Store) saveGenerationNode(n generationNode) error {
	if !safeFileComponent(n.ID) || n.Previous == n.ID {
		return ErrSessionIdentityConflict
	}
	prior, found, err := readJSON[generationNode](s.generationNodePath(n.ID))
	if err != nil {
		return ErrSessionIndexRecoveryRequired
	}
	if found {
		if prior != n {
			return ErrSessionIdentityConflict
		}
		return nil
	}
	return local.Write(s.generationNodePath(n.ID), n)
}

// generationLookup verifies only the tiny head and selected registration. Full
// membership validation is the collector census's job, never a hook scan.
func (s *Store) generationLookup(key agentmeta.SessionKey, id string) error {
	h, found, err := s.loadGenerationHead(key)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if h.Transition != "" {
		return ErrSessionIndexRecoveryRequired
	}
	if h.Retired || h.Active != id {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

// generationRegistrationAllowed closes both journal/head commit windows for
// bounded hook lookup. Completed receipts are tiny; an unfinished journal may
// contain publication bytes, so read only a fixed prefix and fail closed.
func (s *Store) generationRegistrationAllowed(key agentmeta.SessionKey, reg archive.SessionRegistration) error {
	for _, previous := range []string{reg.ArchiveSessionID, reg.PreviousGenerationID} {
		if previous == "" {
			continue
		}
		file, err := os.Open(s.generationRecoveryPath(previous))
		if errors.Is(err, os.ErrNotExist) && previous == reg.ArchiveSessionID {
			continue
		}
		if err != nil {
			return ErrSessionIndexRecoveryRequired
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 4097))
		_ = file.Close()
		var receipt generationRecovery
		if readErr != nil || len(data) > 4096 || json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || !receipt.Complete || receipt.Key != key || receipt.Previous != previous || !safeFileComponent(receipt.Next) || receipt.Next == previous {
			return ErrSessionIndexRecoveryRequired
		}
		if previous == reg.PreviousGenerationID && receipt.Next != reg.ArchiveSessionID {
			return ErrSessionIdentityConflict
		}
	}
	return nil
}

// generationOwner validates explicitly linked duplicates and returns the unique
// active tip. A retired head never authorizes a frozen ancestor. Missing nodes,
// forks, inconsistent registrations and arbitrary duplicate roots fail closed.
func (s *Store) generationOwner(key agentmeta.SessionKey, owners []string) (string, bool, error) {
	h, found, err := s.loadGenerationHead(key)
	if err != nil {
		return "", false, err
	}
	if !found {
		for _, id := range owners {
			reg, present, err := s.LoadRegistration(id)
			if err != nil {
				return "", true, err
			}
			if present && (reg.CaptureFrozen || reg.PreviousGenerationID != "") {
				return "", true, ErrSessionIndexRecoveryRequired
			}
		}
		return "", false, nil
	}
	if h.Transition != "" {
		return "", true, ErrSessionIndexRecoveryRequired
	}
	ancestry, err := s.generationAncestry(h)
	if err != nil {
		return "", true, err
	}
	activeFound := false
	for _, id := range owners {
		n, present := ancestry[id]
		if !present {
			return "", true, ErrSessionIdentityConflict
		}
		reg, present, err := s.LoadRegistration(id)
		if err != nil || !present || reg.PreviousGenerationID != n.Previous || id != h.Active && !reg.CaptureFrozen || id == h.Active && !h.Retired && reg.CaptureFrozen {
			return "", true, ErrSessionIdentityConflict
		}
		if id == h.Active {
			activeFound = true
		}
	}
	if h.Retired {
		return "", true, nil
	}
	if !activeFound {
		e, present, err := s.readQualifiedIndex(key)
		if err == nil && present && e.ArchiveSessionID == h.Active && e.Reservation != "" {
			return h.Active, true, nil
		}
		return "", true, ErrSessionIndexRecoveryRequired
	}
	return h.Active, true, nil
}

func (s *Store) generationAncestry(h generationHead) (map[string]generationNode, error) {
	// Walk the unique active ancestry once; immutable recovery receipts prove
	// each predecessor edge. Separate prior-root edges preserve expired roots
	// without asserting transcript continuity across a genuine new start.
	ancestry := map[string]generationNode{}
	for id := h.Active; id != ""; {
		if _, duplicate := ancestry[id]; duplicate {
			return nil, ErrSessionIdentityConflict
		}
		n, present, err := readJSON[generationNode](s.generationNodePath(id))
		if err != nil || !present || n.Version != 1 || n.Key != h.Key || n.ID != id || n.Previous != "" && n.PreviousRoot != "" {
			return nil, ErrSessionIdentityConflict
		}
		ancestry[id] = n
		if n.Previous != "" {
			r, present, err := readJSON[generationRecovery](s.generationRecoveryPath(n.Previous))
			if err != nil || !present || r.Version != 1 || !r.Complete || r.Key != h.Key || r.Previous != n.Previous || r.Next != id {
				return nil, ErrSessionIdentityConflict
			}
			id = n.Previous
		} else {
			id = n.PreviousRoot
		}
	}
	return ancestry, nil
}

// ensureGenerationReservation starts a new independent root only after an
// active generation expired. Existing admission rules decide whether the
// native event is an eligible genuine start; this helper never admits it.
func (s *Store) ensureGenerationReservation(key agentmeta.SessionKey, id string) error {
	h, found, err := s.loadGenerationHead(key)
	if err != nil || !found {
		return err
	}
	if h.Transition != "" {
		return ErrSessionIndexRecoveryRequired
	}
	if !h.Retired {
		if h.Active != id {
			return ErrSessionIdentityConflict
		}
		return nil
	}
	if _, found, err := s.LoadRegistration(h.Active); err != nil || found {
		return ErrSessionIdentityConflict
	}
	if err := s.saveGenerationNode(generationNode{Version: 1, Key: key, ID: id, PreviousRoot: h.Active}); err != nil {
		return err
	}
	h.Active = id
	h.Retired = false
	return local.Write(s.generationHeadPath(key), h)
}

// stageGenerationRetirement is called before taking a request lock. The
// staged small head commits before registration removal and syncs afterward.
func (s *Store) stageGenerationRetirement(key agentmeta.SessionKey, id string) (*local.Staged, error) {
	if key.NativeID == "" {
		return nil, nil
	}
	h, found, err := s.loadGenerationHead(key)
	if err != nil || !found {
		return nil, err
	}
	if h.Transition != "" {
		return nil, ErrSessionIndexRecoveryRequired
	}
	if h.Active != id || h.Retired {
		return nil, nil
	}
	h.Retired = true
	return local.Stage(s.generationHeadPath(key), h)
}

// FrozenGeneration verifies durable lineage authority before maintenance.
func (s *Store) FrozenGeneration(reg archive.SessionRegistration) error {
	key, err := registrationKey(reg)
	if err != nil {
		return err
	}
	h, found, err := s.loadGenerationHead(key)
	if err != nil || !found || h.Transition != "" || h.Active == reg.ArchiveSessionID && !h.Retired {
		return ErrSessionIndexRecoveryRequired
	}
	n, found, err := readJSON[generationNode](s.generationNodePath(reg.ArchiveSessionID))
	if err != nil || !found || n.Version != 1 || n.Key != key || n.ID != reg.ArchiveSessionID || n.Previous != reg.PreviousGenerationID {
		return fmt.Errorf("frozen generation has no lineage authority: %w", ErrSessionIdentityConflict)
	}
	return nil
}

func (s *Store) generationReservationAllowed(key agentmeta.SessionKey) error {
	if h, found, err := s.loadGenerationHead(key); err != nil {
		return err
	} else if found {
		if h.Transition != "" {
			return ErrSessionIndexRecoveryRequired
		}
		if !h.Retired {
			entry, present, err := s.readQualifiedIndex(key)
			if err != nil || !present || entry.ArchiveSessionID != h.Active {
				return ErrSessionIndexRecoveryRequired
			}
		}
	}
	return nil
}

func (s *Store) recoverGenerationOwners(key agentmeta.SessionKey, owners []string) (bool, error) {
	// An eligible start's durable reservation is also its restart journal. Finish
	// a root rotation interrupted before its node/head write before the census
	// can replace that reservation with the retired tip's absence marker.
	if h, found, err := s.loadGenerationHead(key); err != nil {
		return true, err
	} else if found && h.Retired {
		// Genuine root reservations are always durable per-key overlays. A
		// packed owner is derived from the preceding census and can name an
		// expired tip or belong to the previous packed epoch during rebuilding.
		entry, present, err := readJSON[qualifiedSessionIndexEntry](qualifiedSessionIndexPath(s.home, key))
		if err != nil && !IsUndecodable(err) {
			return true, err
		}
		if present && entry.Reservation != "" && entry.ArchiveSessionID != h.Active {
			if entry.validate(key) != nil {
				return true, ErrSessionIdentityConflict
			}
			if err := s.ensureGenerationReservation(key, entry.ArchiveSessionID); err != nil {
				return true, err
			}
		}
	}
	if active, protected, err := s.generationOwner(key, owners); err != nil {
		return true, err
	} else if protected {
		if active == "" {
			h, found, err := s.loadGenerationHead(key)
			if err != nil || !found {
				return true, ErrSessionIndexRecoveryRequired
			}
			return true, s.writeGenerationIndexUnderRequestLock(h.Active, qualifiedSessionIndexPath(s.home, key), func() error {
				current, protected, err := s.generationOwner(key, owners)
				if err != nil {
					return err
				}
				if !protected || current != "" {
					return ErrSessionIndexRecoveryRequired
				}
				return nil
			}, func(fileSnapshot) (any, bool, error) {
				return qualifiedSessionIndexEntry{Version: 1, Agent: key.Agent, NativeID: key.NativeID, Absent: true}, true, nil
			})
		}
		if _, registered, err := s.LoadRegistration(active); err != nil {
			return true, err
		} else if !registered {
			return true, nil
		} // Valid root reservation remains untouched.
		return true, s.writeGenerationIndexUnderRequestLock(active, qualifiedSessionIndexPath(s.home, key), func() error {
			current, protected, err := s.generationOwner(key, owners)
			if err != nil {
				return err
			}
			if !protected || current != active {
				return ErrSessionIndexRecoveryRequired
			}
			return nil
		}, func(current fileSnapshot) (any, bool, error) {
			if current.found {
				var prior qualifiedSessionIndexEntry
				if json.Unmarshal(current.data, &prior) == nil && prior.Reservation != "" && prior.ArchiveSessionID != active {
					return nil, false, ErrSessionIdentityConflict
				}
			}
			return indexEntry(key, active), true, nil
		})
	}
	return false, nil
}

// A validated journal or census owns generation routing even while its packed
// predecessor is frozen. Compare raw logical index bytes under the request lock;
// the caller checks lineage authority and allowable owners before replacement.
// Keep packed overlay expectation and epoch fencing for the resulting write.
func (s *Store) writeGenerationIndexUnderRequestLock(id, path string, check func() error, change func(fileSnapshot) (any, bool, error)) error {
	indexStore := *s
	indexStore.onWriteSync = s.onIndexSync
	indexStore.indexSnapshots = true
	return indexStore.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return s.lockRequest(id) }, path: path, check: check, change: change,
		snapshot: func(path string) (fileSnapshot, error) { return s.readQualifiedIndexSnapshot(path, false) },
	})
}

// A previously queued child retains its frozen parent. Candidate authority is
// separate from active native routing and must never recreate a missing parent.
func (s *Store) matchingCandidateParent(key agentmeta.SessionKey, id string) (bool, error) {
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found {
		return false, err
	}
	if !reg.CaptureFrozen {
		return s.matchingRegistration(key, id)
	}
	actual, err := registrationKey(reg)
	if err != nil || actual != key || reg.ArchiveSessionID != id || reg.Validate() != nil {
		return false, ErrSessionIdentityConflict
	}
	if err := s.generationRegistrationAllowed(key, reg); err != nil {
		return false, err
	}
	return true, s.FrozenGeneration(reg)
}

// GenerationCaptureAllowed checks the bounded active head before a collector
// can read native evidence. A retirement committed before registration deletion
// cannot resurrect expired content after a crash.
func (s *Store) GenerationCaptureAllowed(reg archive.SessionRegistration) error {
	key, err := registrationKey(reg)
	if err != nil {
		return err
	}
	h, found, err := s.loadGenerationHead(key)
	if err != nil {
		return err
	}
	if found && (h.Retired || h.Transition != "" || h.Active != reg.ArchiveSessionID) {
		return ErrSessionIndexRecoveryRequired
	}
	if !found && reg.PreviousGenerationID != "" {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

// generationCommittedAuthority binds frozen routing to the exact complete prior selection.
func (s *Store) generationCommittedAuthority(reg archive.SessionRegistration) (string, string, error) {
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		return "", "", err
	}
	prior := p.PublicationPredecessor()
	if prior.State != PredecessorPresent {
		return "", "", errors.New("generation recovery requires exact prior publication authority")
	}
	if _, err := p.CommittedSources(); err != nil {
		return "", "", err
	}
	digest, _, err := archive.PublicationIdentity(prior.Body, reg.DestinationID, "generation_recovery", "", "generation_recovery")
	if err != nil {
		return "", "", err
	}
	return publicationSHA256(prior.Body), digest, nil
}
