package backfill

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// materialize uses one provider snapshot for header, identity and privacy
// interpretation. No source bytes are read while hooks.lock is held.
func (r Registration) materialize(ctx context.Context, c Candidate, reg *archive.SessionRegistration, child *Subagent, supplemental []archive.SupplementalEvidence) (out archive.SourceBundle, err error) {
	if c.sourceAdmissionCurrent != nil && !c.sourceAdmissionCurrent() {
		return out, errors.New("durable import reviewed file identity or creation changed")
	}
	provider, filter, ok := r.Sources.LookupSources(c.Harness)
	if !ok {
		return out, errors.New("durable import source unavailable")
	}
	bounded, ok := provider.(agentapi.AdmissionSourceProvider)
	if !ok {
		return out, errors.New("durable import requires a bounded admission source capability")
	}
	ref := agentapi.SourceRef{Kind: c.SourceKind, Path: c.TranscriptPath, Key: c.SourceKey}
	pass, err := bounded.OpenAdmissionPass(ctx, agentapi.SourceEnvironment{Database: r.CursorDatabase}, ref)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, pass.Close()) }()
	limits := agentapi.ReadLimits{RawBytes: collector.DefaultMaxRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes, SubagentMetadata: child != nil}
	snap, err := pass.Read(ctx, ref, limits)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, snap.Close()) }()
	if c.SourceKind == archive.SourceKindCursorSQLite {
		facts, ok := snap.(agentapi.AdmissionCatalogSnapshot)
		if !ok || c.reviewedChat == nil {
			return out, errors.New("durable Cursor import reviewed identity facts unavailable")
		}
		chat := facts.AdmissionChat()
		if chat != *c.reviewedChat || chat.ID != c.NativeSessionID || chat.KeyID != c.SourceKey || !chat.CreatedAt.Equal(c.StartedAt) || chat.Malformed {
			return out, errors.New("durable Cursor import identity, workspace or selection changed; review again")
		}
	}
	input := snap.Input()
	var header agentapi.NativeHeader
	if input.File != nil && child == nil {
		header, err = r.admissionHeader(ctx, c, ref, input.File, limits)
		if err != nil {
			return out, err
		}
	}
	filtered, err := filter.Filter(ctx, input, agentapi.FilterContext{Filename: ref.Path, StartedAt: c.StartedAt, Limits: limits})
	if err != nil {
		return out, err
	}
	if int64(filtered.Boundary.RetainedBytes) > state.AdmissionStageFilteredLimit {
		return out, archive.ErrSourceTooLarge
	}
	if input.File != nil {
		if err = input.File.Check(); err != nil {
			return out, err
		}
	}
	if c.sourceAdmissionCurrent != nil && !c.sourceAdmissionCurrent() {
		return out, errors.New("durable import reviewed file identity or creation changed")
	}
	if child != nil {
		if err = collector.CheckImportedSubagent(filtered, reg.ParentNativeSessionID, child.AgentID, c.StartedAt, r.AdmittedAt); err != nil {
			return out, err
		}
		reg.SessionStartedAt = filtered.NativeStartAt
	} else {
		imports, ok := r.Sources.(agentapi.ImportsLookup)
		if !ok {
			return out, errors.New("durable import identity capability unavailable")
		}
		inspector, ok := imports.LookupImport(c.Harness)
		if !ok {
			return out, errors.New("durable import identity inspection unavailable")
		}
		inspection, e := inspector.InspectImport(ctx, agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.ID(archive.CanonicalHarness(c.Harness)), NativeID: c.NativeSessionID}, Source: ref, Header: header, Filtered: filtered})
		if e != nil {
			return out, e
		}
		if !inspection.Conversation || inspection.IdentityMismatch || (!inspection.StartedAt.IsZero() && !inspection.StartedAt.Equal(c.StartedAt)) {
			return out, errors.New("durable import selected conversation changed")
		}
	}
	return archive.NewSourceBundle(*reg, filter, filtered, r.AdmittedAt, supplemental)
}

func (r Registration) admissionHeader(ctx context.Context, c Candidate, ref agentapi.SourceRef, file agentapi.FileInput, limits agentapi.ReadLimits) (header agentapi.NativeHeader, err error) {
	lookup, ok := r.Sources.(agentapi.NativeHeadersLookup)
	if !ok {
		return header, errors.New("durable import native header capability unavailable")
	}
	inspector, ok := lookup.LookupNativeHeaders(c.Harness)
	if !ok {
		// Some native layouts (Cursor file exports) have no header codec:
		// their reviewed identity is the native layout plus the captured file.
		imports, supported := r.Sources.(agentapi.ImportsLookup)
		if !supported {
			return header, errors.New("durable import native header unavailable")
		}
		declared, supported := imports.LookupImport(c.Harness)
		if !supported || declared.ImportPolicy(ref).Start != agentapi.ImportFileCreatedStart || c.reviewedHeader == nil || *c.reviewedHeader != (agentapi.NativeHeader{}) {
			return header, errors.New("durable import native header unavailable")
		}
	} else {
		header, err = inspector.InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryImport, Path: ref.Path, Scan: func(fn func([]byte) bool) error {
			_, err := file.Records(ctx, true, limits.RawBytes, limits.RecordBytes, fn)
			return err
		}})
		if err != nil {
			return header, err
		}
		if header.IdentityMismatch || header.SubagentOnly || header.CapturePending != "" || (header.NativeID != "" && header.NativeID != c.NativeSessionID) {
			return header, errors.New("durable import native header changed")
		}
	}
	if c.reviewedHeader != nil && (header.Directory != c.reviewedHeader.Directory || header.RepoKey != c.reviewedHeader.RepoKey || !header.StartedAt.Equal(c.reviewedHeader.StartedAt)) {
		return header, errors.New("durable import reviewed ownership changed")
	}
	return header, nil
}

func (r Registration) prepareWork(ctx context.Context, cfg config.Config, w *parentWork, result *RegistrationResult) error {
	if w.prepared || w.finished {
		return nil
	}
	skip, err := r.skip(cfg, w, result)
	if err != nil {
		return err
	}
	if skip || !r.valid(w.c) {
		w.finished = true
		return nil
	}
	key := agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(w.c.Harness)), NativeID: w.c.NativeSessionID}
	w.id, _, err = r.Store.EnsureArchiveSessionID(key)
	if err != nil {
		return err
	}
	reg := r.registration(w.c, w.id, w.repoKey)
	if len(w.c.Subagents) > 64 {
		return errors.New("durable import child group exceeds 64 selected children; reduce the group before retry")
	}
	if !cfg.DurableImportProtection {
		return errors.New("durable import writer fence was not committed")
	}
	if prior, retained, digest, found, e := r.Store.PreparedAdmissionStage(w.id); e != nil {
		return e
	} else if found {
		current := reg
		current.AdmittedAt = prior.Reservation.AdmittedAt
		current.RegisteredAt = prior.Reservation.RegisteredAt
		if state.CheckAdmissionStageOwnership(current, prior) != nil || prior.SkillEvidence != string(cfg.EffectiveSkillEvidence()) || !r.stagePrivacyCurrent(retained) {
			return state.ErrAdmissionStageRecovery
		}
		reg = prior.Reservation
		reg.AdmissionStage = digest
		w.stagedRegistration = reg
		w.prepared = true
		w.stageSkillEvidence = prior.SkillEvidence
		w.next = len(w.c.Subagents)
		w.childRegistrations = prior.Children
		for _, child := range prior.Children {
			w.children = append(w.children, child.ArchiveSessionID)
		}
		return nil
	}
	if r.Sources == nil {
		return errors.New("durable import source integrations unavailable")
	}
	// Every selected child gets its own stage and admission. It is never made
	// native-loss-safe merely by being linked in the parent's evidence.
	for _, sub := range w.c.Subagents {
		if err := r.prepareChild(ctx, cfg, w, key.Agent, reg, sub); err != nil {
			return err
		}
	}
	for _, id := range w.children {
		link, e := archive.NewLinkedSessionEvidence(id, archive.LinkedSessionPending, r.AdmittedAt)
		if e != nil {
			return e
		}
		w.links = append(w.links, link)
	}
	bundle, err := r.materialize(ctx, w.c, &reg, nil, w.links)
	if err != nil {
		return fmt.Errorf("stage selected parent: %w", err)
	}
	digest, err := r.Store.PrepareAdmissionStage(reg, bundle, string(cfg.EffectiveSkillEvidence()), r.AdmittedAt, w.childRegistrations...)
	if err != nil {
		return err
	}
	reg.AdmissionStage = digest
	w.stagedRegistration = reg
	w.prepared = true
	w.stageSkillEvidence = string(cfg.EffectiveSkillEvidence())
	w.next = len(w.c.Subagents)
	return nil
}

func (r Registration) prepareChild(ctx context.Context, cfg config.Config, w *parentWork, agent agentmeta.ID, reg archive.SessionRegistration, sub Subagent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	native := w.c.NativeSessionID + ":subagent:" + sub.AgentID
	id, _, e := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agent, NativeID: native})
	if e != nil {
		return e
	}
	existing, found, e := r.Store.LoadRegistration(id)
	if e != nil {
		return e
	}
	if found {
		if existing.ParentSessionID != w.id || existing.ProjectRoot != w.c.ProjectRoot || existing.NativeSessionID != native || existing.DestinationID != r.DestinationID {
			return state.ErrSessionIdentityConflict
		}
		if existing.AdmissionStage == "" {
			return errors.New("selected child has no durable evidence")
		}
		w.children = append(w.children, id)
		return nil
	}
	childReg := reg
	childReg.ArchiveSessionID = id
	childReg.NativeSessionID = native
	childReg.TranscriptPath = sub.Path
	childReg.ParentSessionID = w.id
	childReg.ParentNativeSessionID = w.c.NativeSessionID
	childReg.SubagentID = sub.AgentID
	childReg.SubagentObservedAt = r.AdmittedAt
	childReg.StartedAtSource = archive.StartedAtSourceTranscript
	if prior, retained, digest, found, e := r.Store.PreparedAdmissionStage(id); e != nil {
		return e
	} else if found {
		childReg.SessionStartedAt = prior.Reservation.SessionStartedAt
		childReg.AdmittedAt = prior.Reservation.AdmittedAt
		childReg.RegisteredAt = prior.Reservation.RegisteredAt
		if state.CheckAdmissionStageOwnership(childReg, prior) != nil || prior.SkillEvidence != string(cfg.EffectiveSkillEvidence()) || !r.stagePrivacyCurrent(retained) {
			return state.ErrAdmissionStageRecovery
		}
		childReg = prior.Reservation
		childReg.AdmissionStage = digest
		w.childRegistrations = append(w.childRegistrations, childReg)
		w.children = append(w.children, id)
		return nil
	}
	childCandidate := w.c
	childCandidate.TranscriptPath = sub.Path
	childCandidate.SourceKind = archive.SourceKindFile
	childCandidate.SourceKey = ""
	bundle, e := r.materialize(ctx, childCandidate, &childReg, &sub, nil)
	if e != nil {
		return fmt.Errorf("stage selected child: %w", e)
	}
	// The bundle's captured start was verified by CheckImportedSubagent.
	// Native start is set from the verified filtered child.
	digest, e := r.Store.PrepareAdmissionStage(childReg, bundle, string(cfg.EffectiveSkillEvidence()), r.AdmittedAt)
	if e != nil {
		return e
	}
	childReg.AdmissionStage = digest
	w.childRegistrations = append(w.childRegistrations, childReg)
	w.children = append(w.children, id)
	return nil
}

func (r Registration) stagePrivacyCurrent(bundle archive.SourceBundle) bool {
	if r.Sources == nil {
		return false
	}
	_, filter, ok := r.Sources.LookupSources(bundle.Capture.Harness.Name)
	return ok && bundle.Capture.FilterVersion == archive.FilterVersion && bundle.Capture.AdapterVersion == filter.Version()
}

// admissionPreparationError exposes an action without leaking native paths,
// locators or decoder payloads. The cause remains available to typed callers.
type admissionPreparationError struct {
	source archive.SourceKind
	cause  error
}

func (e admissionPreparationError) Error() string {
	if errors.Is(e.cause, state.ErrAdmissionStageCapacity) {
		return state.ErrAdmissionStageCapacity.Error()
	}
	if errors.Is(e.cause, state.ErrAdmissionStageRecovery) {
		return state.ErrAdmissionStageRecovery.Error()
	}
	if errors.Is(e.cause, archive.ErrSourceTooLarge) {
		return "import source exceeds bounded durable evidence capacity; retry with supported bounded capacity"
	}
	if e.source == archive.SourceKindCursorSQLite {
		return "Cursor import remains unadmitted; settle the database and review backfill again, or retry after bounded snapshot support is available"
	}
	return "import source remains unadmitted; restore readable unchanged evidence and review backfill again"
}
func (e admissionPreparationError) Unwrap() error { return e.cause }
