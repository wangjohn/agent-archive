package state

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// compositionGenerationFixture is generationFixture on a home that requires
// selecting authority for every new pending publication.
func compositionGenerationFixture(t *testing.T) (*Store, archive.SessionRegistration, time.Time) {
	t.Helper()
	s, reg, at := generationFixture(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.DestinationID = cfg.DestinationID()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, _, err = s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WithPublicationComposition(s.Home(), func(config.PublicationCompositionGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return s, reg, at
}

// selectableGenerationBuilder renders what the collector builder renders on a
// protected home: current skill policy and adapter facts in the metadata.
func selectableGenerationBuilder(t *testing.T, s *Store, at time.Time, clearStage bool) func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error) {
	t.Helper()
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	build := generationBuilder(at)
	return func(reg archive.SessionRegistration, id string) (archive.SessionRegistration, PendingPublication, error) {
		if clearStage {
			reg.StagedImportSHA256 = ""
		}
		reg, pending, err := build(reg, id)
		if err != nil {
			return reg, pending, err
		}
		var metadata archive.Metadata
		if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
			return reg, pending, err
		}
		metadata.Adapter = archive.AdapterInfo{Name: pending.Bundle.Capture.AdapterName, Version: pending.Bundle.Capture.AdapterVersion}
		pending.MetadataBytes, err = json.Marshal(metadata)
		pending.SkillEvidence = string(cfg.EffectiveSkillEvidence())
		return reg, pending, err
	}
}

// assertSelectedSuccessor checks the completed transition: the successor is
// routed, owns an ordinary ready selection and the predecessor is frozen.
func assertSelectedSuccessor(t *testing.T, s *Store, original archive.SessionRegistration) PendingPublication {
	t.Helper()
	next, found, err := s.GenerationSuccessor(original.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("successor receipt: %v %v", found, err)
	}
	key, err := registrationKey(original)
	if err != nil {
		t.Fatal(err)
	}
	if active, found, err := s.ArchiveSessionID(key); err != nil || !found || active != next {
		t.Fatalf("successor not routed: %s %v %v", active, found, err)
	}
	old, found, err := s.LoadRegistration(original.ArchiveSessionID)
	if err != nil || !found || !old.CaptureFrozen || old.StagedImportSHA256 != original.StagedImportSHA256 {
		t.Fatalf("predecessor: %#v %v %v", old, found, err)
	}
	pending, found, err := s.LoadPending(next)
	if err != nil || !found || pending.Selection == nil || pending.Selection.Predecessor != ReadyPredecessorAbsent || pending.StagedProof() != nil {
		t.Fatalf("successor pending: %#v %v %v", pending.Selection, found, err)
	}
	if err := s.ValidateReadySelection(t.Context(), pending); err != nil {
		t.Fatalf("successor selection: %v", err)
	}
	return pending
}

func TestGenerationRecoverySelectsSuccessorOnCompositionHome(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(t, s, at, false)); err != nil {
		t.Fatal(err)
	}
	pending := assertSelectedSuccessor(t, s, reg)
	published, err := s.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(pending, at); err != nil {
		t.Fatalf("successor publication: %v", err)
	}
	if err := s.RemovePending(pending.Bundle.ArchiveSessionID); err != nil {
		t.Fatalf("acknowledged successor cleanup: %v", err)
	}
}

// Replay derives the selection again; every interrupted step saves the same
// pending bytes the uninterrupted transition does.
func TestGenerationRecoverySuccessorSelectionReplaysIdentically(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"generation-journal", "generation-frozen", "generation-registration", "generation-pending", "generation-request", "generation-index"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			s, reg, at := compositionGenerationFixture(t)
			interrupted := errors.New("interrupted")
			var saved []byte
			fired := false
			s.onIndexStep = func(current string) error {
				if current == "generation-pending" && saved == nil {
					next, _, _ := s.GenerationSuccessor(reg.ArchiveSessionID)
					saved, _ = os.ReadFile(s.pendingPath(next))
				}
				if current == step && !fired {
					fired = true
					return interrupted
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(t, s, at, false)); !errors.Is(err, interrupted) {
				t.Fatalf("did not interrupt %s: %v", step, err)
			}
			if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
				t.Fatalf("replay after %s: %v", step, err)
			}
			pending := assertSelectedSuccessor(t, s, reg)
			replayed, err := os.ReadFile(s.pendingPath(pending.Bundle.ArchiveSessionID))
			if err != nil || len(saved) == 0 || string(replayed) != string(saved) {
				t.Fatalf("replayed pending differs from the first save: %v", err)
			}
		})
	}
}

// A successor that cannot take selecting authority is refused while beginning,
// before a journal exists, a routing fence is written or the predecessor freezes.
func TestGenerationRecoveryRefusesUnselectableSuccessorBeforeJournal(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	before := stagedGenerationInventory(t, s.Home())
	// generationBuilder carries no current skill policy, so no ready selection binds it.
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("unselectable successor: %v", err)
	}
	if after := stagedGenerationInventory(t, s.Home()); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused recovery changed durable state:\n%v\n%v", before, after)
	}
}

// Configuration that changes after the journal refuses replay before fencing;
// restoring it lets the same journal complete.
func TestGenerationRecoveryReplayRefusesUnselectableSuccessorBeforeFreeze(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	interrupted := errors.New("interrupted")
	s.onIndexStep = func(current string) error {
		if current == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(t, s, at, false)); !errors.Is(err, interrupted) {
		t.Fatalf("did not interrupt at the journal: %v", err)
	}
	s.onIndexStep = nil
	setSkillEvidence := func(mode config.SkillEvidence) {
		t.Helper()
		cfg, _, err := config.Load(s.Home())
		if err != nil {
			t.Fatal(err)
		}
		cfg.SkillEvidence = mode
		if err := config.Save(s.Home(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	setSkillEvidence(config.SkillEvidenceMetadata)
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("replay with changed skill policy: %v", err)
	}
	old, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || old.CaptureFrozen {
		t.Fatalf("refused replay froze the predecessor: %v %v", found, err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}
	if h, present, err := s.loadGenerationHead(key); err != nil || present {
		t.Fatalf("refused replay fenced routing: %#v %v", h, err)
	}
	setSkillEvidence(config.SkillEvidenceBody)
	if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertSelectedSuccessor(t, s, reg)
}

func TestGenerationSuccessorWithHistoryHasNoOrdinarySelection(t *testing.T) {
	t.Parallel()
	s, reg, at := compositionGenerationFixture(t)
	successor, pending, err := selectableGenerationBuilder(t, s, at, false)(reg, "synthetic-successor")
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PendingPublication){
		"pending history": func(p *PendingPublication) { p.History = &PendingHistory{Version: 1} },
		"bundle history":  func(p *PendingPublication) { p.Bundle.History = &archive.SourceHistory{} },
		"history journal": func(p *PendingPublication) { p.JournalVersion = 5 },
	} {
		candidate := pending
		mutate(&candidate)
		if _, err := s.generationSuccessorPending(t.Context(), successor, candidate); !errors.Is(err, ErrReadySelectionFrozen) {
			t.Fatalf("%s successor selected: %v", name, err)
		}
	}
}

// The whole staged path: a typed staged selection released through readback,
// blocked as rewritten, then recovered into an ordinary selected successor.
func TestGenerationRecoveryCompletesOnStagedHome(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, selectableGenerationBuilder(t, s, at, true)); err != nil {
		t.Fatal(err)
	}
	pending := assertSelectedSuccessor(t, s, reg)
	successor, found, err := s.LoadRegistration(pending.Bundle.ArchiveSessionID)
	if err != nil || !found || successor.StagedImportSHA256 != "" || successor.PreviousGenerationID != reg.ArchiveSessionID {
		t.Fatalf("successor registration: %#v %v %v", successor, found, err)
	}
	published, err := s.LoadPublishedState(successor.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(pending, at); err != nil {
		t.Fatalf("successor publication: %v", err)
	}
}
