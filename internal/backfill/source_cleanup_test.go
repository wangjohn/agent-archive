package backfill

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestDatabaseLimitCannotHideCleanupFailure(t *testing.T) {
	fault := errors.New("synthetic raw snapshot cleanup fault")
	read := func(context.Context, string) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
		return cursorstore.Composer{}, nil, errors.Join(agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge), agentapi.Wrap(agentapi.Cleanup, fault))
	}
	work := []*work{{chat: CursorDatabaseChat{KeyID: "synthetic"}}}
	if err := readCursorDatabaseChats(t.Context(), Environment{Imports: testSources}, 2, nil, read, work, testSources); !errors.Is(err, fault) {
		t.Fatalf("cleanup was hidden by chat limit: %v", err)
	}
}

func TestFilePlanCannotHideCleanupFailure(t *testing.T) {
	fault := errors.New("synthetic file close fault")
	env := Environment{Sources: cleanupSources{fault: fault}, Imports: testSources}
	w := &work{t: &transcript{harness: harnessCodex, path: "/synthetic/native.jsonl", size: 3}, c: Candidate{NativeSessionID: "synthetic"}}
	if err := classifyPlanWork(t.Context(), env, states{}, Filters{}, []*work{w}, nil, time.Time{}, time.Time{}, 2); !errors.Is(err, fault) {
		t.Fatalf("file cleanup became a successful plan: %v", err)
	}
}

type cleanupSources struct{ fault error }

func (s cleanupSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	_, filter, ok := testSources.LookupSources(name)
	return cleanupProvider{fault: s.fault}, filter, ok
}

type cleanupProvider struct {
	sourceio.FileProvider
	fault error
}

func (p cleanupProvider) OpenPass(context.Context, agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return cleanupPass{fault: p.fault}, nil
}

type cleanupPass struct {
	agentapi.SourcePass
	fault error
}

func (p cleanupPass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	return nil, errors.Join(agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge), agentapi.Wrap(agentapi.Cleanup, p.fault))
}

func (cleanupPass) Close() error { return nil }

func TestRegistrationSourceObservationCannotHideCleanupFailure(t *testing.T) {
	t.Parallel()
	for name, signatureErr := range map[string]error{"observed": nil, "missing": os.ErrNotExist} {
		t.Run(name, func(t *testing.T) {
			fault := errors.New("synthetic observation owner cleanup fault")
			sources := observationCleanupSources{fault: fault, signatureErr: signatureErr}
			candidate := Candidate{Harness: "cursor", NativeSessionID: "synthetic", SourceKind: archive.SourceKindCursorSQLite, SourceKey: "synthetic"}
			r := Registration{Sources: sources, Home: t.TempDir(), CursorDatabase: "/synthetic/state.vscdb"}
			result, err := r.Run([]Candidate{candidate})
			if !errors.Is(err, fault) || result.Gone != 0 || len(result.Sessions) != 0 {
				t.Fatalf("registration hid observation cleanup: %+v %v", result, err)
			}
		})
	}
}

func TestUndoSourceObservationCannotHideCleanupFailure(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, signatureErr := range map[string]error{"observed": nil, "missing": os.ErrNotExist} {
		t.Run(name, func(t *testing.T) {
			fault := errors.New("synthetic observation owner cleanup fault")
			sources := observationCleanupSources{fault: fault, signatureErr: signatureErr}
			reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", Harness: archive.Harness{Name: "cursor"}, NativeSessionID: "synthetic", SourceKind: archive.SourceKindCursorSQLite, SourceKey: "synthetic", AdmittedAt: fixedNow}
			resumed, unknown, err := resumedSinceImport(Environment{Sources: sources}, store, reg, state.Request{})
			if !errors.Is(err, fault) || resumed || unknown {
				t.Fatalf("undo hid observation cleanup: resumed=%v unknown=%v err=%v", resumed, unknown, err)
			}
		})
	}
}

type observationCleanupSources struct {
	fault        error
	signatureErr error
}

func (s observationCleanupSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	return observationCleanupProvider{SourceProvider: p, observationCleanupSources: s}, f, ok
}

type observationCleanupProvider struct {
	agentapi.SourceProvider
	observationCleanupSources
}

func (p observationCleanupProvider) OpenPass(context.Context, agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return observationCleanupPass{observationCleanupSources: p.observationCleanupSources}, nil
}

type observationCleanupPass struct {
	agentapi.SourcePass
	observationCleanupSources
}

func (p observationCleanupPass) Signature(context.Context, agentapi.SourceRef) (agentapi.SourceObservation, error) {
	return agentapi.SourceObservation{}, p.signatureErr
}

func (p observationCleanupPass) Close() error { return agentapi.Wrap(agentapi.Cleanup, p.fault) }
