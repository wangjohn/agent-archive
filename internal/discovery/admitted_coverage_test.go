package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestAdmittedCoverageSliceDoesNotAdmitOtherOwners(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	other := writeRollout(t, root, project, at.Add(time.Minute), 2, "sessions")
	reg := archive.SessionRegistration{ArchiveSessionID: "admitted", NativeSessionID: wanted, Harness: archive.Harness{Name: "codex"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), Origin: archive.SessionOriginHook, AdmittedAt: at.Add(time.Minute), RegisteredAt: at.Add(time.Minute), SessionStartedAt: at.Add(time.Minute), DestinationID: cfg.DestinationID(), TranscriptPath: filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+wanted+".jsonl")}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	budget := l.NativeReadBudget()
	cfg.Discovery.Enabled = false
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{reg}, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}); err != nil {
		t.Fatal(err)
	}
	set, err := l.Thread(t.Context(), wanted)
	if err != nil || !set.Complete || len(set.Candidates) != 1 {
		t.Fatalf("qualified admitted source: %+v %v", set, err)
	}
	if l.NativeReadBudget() != budget {
		t.Fatal("replaced shared budget")
	}
	if _, found := l.coverage.Requests[other]; found {
		t.Fatal("unadmitted source requested")
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("inventory admitted a new owner: %+v %v", regs, err)
	}
	cfg.Harnesses = []string{"claude"}
	excluded := reg
	excluded.NativeSessionID = other
	sequence := l.coverage.Sequence
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{excluded}, Options{}); err != nil {
		t.Fatal(err)
	}
	if sequence != l.coverage.Sequence {
		t.Fatal("excluded owner requested")
	}
	cfg.Harnesses = []string{"codex"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	missing := reg
	missing.NativeSessionID = "00000000-0000-0000-0000-000000000003"
	if err := l.PrepareRegistered(ctx, cfg, []archive.SessionRegistration{missing}, Options{}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if request := l.coverage.Requests[missing.NativeSessionID]; request.CompleteEpoch == l.coverage.Epoch {
		t.Fatal("cancellation invented complete coverage")
	}
}
