package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestSubagentSourceFailuresPreserveRetryAndCause(t *testing.T) {
	fault := errors.New("synthetic source fault")
	for _, tc := range []struct {
		name      string
		read      error
		close     bool
		permanent bool
	}{
		{name: "unavailable", read: agentapi.Wrap(agentapi.Unavailable, fault)},
		{name: "changed", read: agentapi.Wrap(agentapi.Changed, fault)},
		{name: "missing with cleanup", read: errors.Join(agentapi.Wrap(agentapi.Missing, os.ErrNotExist), agentapi.Wrap(agentapi.Cleanup, fault))},
		{name: "limit with cleanup", read: errors.Join(agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit), agentapi.Wrap(agentapi.Cleanup, fault))},
		{name: "builtin file close", close: true},
		{name: "deterministic limit", read: agentapi.Wrap(agentapi.Limit, fault), permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store, err := openTestStore(home)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
			path := filepath.Join(home, "child.jsonl")
			if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
			if err := store.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "child-native", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: parent.Harness, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
			if err := store.SaveSubagentCandidate(candidate); err != nil {
				t.Fatal(err)
			}
			closes := 0
			var files transcriptio.Opener
			if tc.close {
				files = candidateCloseFiles{fault: fault, closes: &closes}
			}
			sources := candidateFaultSources{read: tc.read, files: files}
			outcome := materializeSubagentCandidates(t.Context(), store, Options{Sources: sources}, candidate.ObservedAt.Add(subagentTranscriptGrace))
			if tc.close && closes != 1 {
				t.Fatalf("builtin descriptor closed %d times", closes)
			}
			got := outcome.errors["child"]
			if !errors.Is(got, fault) || !errors.Is(got, ErrSubagentCandidate) {
				t.Fatalf("lost source cause or candidate classification: %v", got)
			}
			if errors.Is(got, ErrSubagentNotCaptured) != tc.permanent {
				t.Fatalf("permanent=%v error=%v", tc.permanent, got)
			}
			pending, err := store.LoadSubagentCandidates()
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.permanent {
				want = 0
			}
			if len(pending) != want {
				t.Fatalf("pending=%v, want count %d", pending, want)
			}
			requests, err := store.LoadRequests()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.permanent && len(requests) != 0 {
				t.Fatalf("retryable failure marked parent unavailable: %v", requests)
			}
			if _, found, err := store.LoadRegistration("child"); err != nil || found {
				t.Fatalf("failed source registered: %v %v", found, err)
			}
			if !tc.permanent {
				retry := materializeSubagentCandidates(t.Context(), store, Options{Sources: testSources}, candidate.ObservedAt)
				if len(retry.errors) != 0 || len(retry.rejected) != 0 {
					t.Fatalf("retry failed: %+v", retry)
				}
				if _, found, err := store.LoadRegistration("child"); err != nil || !found {
					t.Fatalf("retry did not register: %v %v", found, err)
				}
			}
		})
	}
}

type candidateFaultSources struct {
	read  error
	files transcriptio.Opener
}

func (s candidateFaultSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	return candidateFaultProvider{SourceProvider: p, read: s.read, files: s.files}, f, ok
}

type candidateFaultProvider struct {
	agentapi.SourceProvider
	read  error
	files transcriptio.Opener
}

func (p candidateFaultProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	e.Files = p.files
	pass, err := p.SourceProvider.OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	return candidateFaultPass{SourcePass: pass, read: p.read}, nil
}

type candidateFaultPass struct {
	agentapi.SourcePass
	read error
}

func (p candidateFaultPass) Read(ctx context.Context, r agentapi.SourceRef, l agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.read != nil {
		return nil, p.read
	}
	return p.SourcePass.Read(ctx, r, l)
}

type candidateCloseFiles struct {
	transcriptio.OS
	fault  error
	closes *int
}

func (f candidateCloseFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return candidateCloseFile{File: file, fault: f.fault, closes: f.closes}, nil
}

type candidateCloseFile struct {
	transcriptio.File
	fault  error
	closes *int
}

func (f candidateCloseFile) Close() error {
	*f.closes++
	return errors.Join(f.File.Close(), f.fault)
}
