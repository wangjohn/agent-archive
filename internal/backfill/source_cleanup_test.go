package backfill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
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
