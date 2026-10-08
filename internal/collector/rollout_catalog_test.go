package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPendingCatalogIsLazySharedAndDoesNotAdmitHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		const other = "22222222-2222-4222-8222-222222222222"
		home := t.TempDir()
		dir := filepath.Join(home, "sessions")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic/project","timestamp":"2026-10-01T12:00:00Z","cli_version":"0.160.0","originator":"codex_cli_rs","source":"cli"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"synthetic"}}` + "\n"
		ordinary := writeTranscript(t, dir, "rollout-"+id+".jsonl", raw)
		related := writeTranscript(t, dir, "rollout-"+other+".jsonl", raw)
		c := newCollectorCatalog(t, []string{home})
		calls := 0
		opts := Options{Sources: testSources, PendingCodexRollouts: func() agentapi.CodexRolloutLookup { calls++; return c }}
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: ordinary}
		_, filter, _ := testSources.LookupSources("codex")
		reader, ok := newSourceReader(reg, opts)
		if !ok {
			t.Fatal("source reader missing")
		}
		if _, err := reader.Signature(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.Filter(t.Context(), filter, DefaultMaxTranscriptBytes); err != nil {
			t.Fatal(err)
		}
		if calls != 0 || c.sweeps != 0 {
			t.Fatalf("ordinary capture enumerated history: calls %d counters %#v", calls, c.sweeps)
		}
		reg.TranscriptPath = related
		reader, ok = newSourceReader(reg, opts)
		if !ok {
			t.Fatal("source reader missing")
		}
		for range 2 {
			if _, err := reader.Signature(t.Context()); !errors.Is(err, archive.ErrRelatedHistory) || !strings.Contains(err.Error(), "candidate rollout locators") {
				t.Fatalf("history fence lost: %v", err)
			}
		}
		if calls != 2 || c.sweeps != 2 {
			t.Fatalf("pending views not shared metadata observation: calls %d counters %#v", calls, c.sweeps)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := reader.(providerReader).observePendingHistory(ctx, archive.ErrRelatedHistory)
		if !errors.Is(err, archive.ErrRelatedHistory) || !strings.Contains(err.Error(), "locator evidence unavailable") {
			t.Fatalf("cancelled fence lost: %v", err)
		}
	})
}

func TestPendingCatalogPreservesTypedFailureAndReportsOnlyCounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const id = "11111111-1111-4111-8111-111111111111"
		homes := []string{t.TempDir(), t.TempDir()}
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic/project","timestamp":"2026-10-01T12:00:00Z","cli_version":"0.160.0","originator":"codex_cli_rs","source":"cli"}}` + "\n" + `{"private":"synthetic-private-body"}` + "\n"
		var path string
		for _, home := range homes {
			dir := filepath.Join(home, "sessions")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path = writeTranscript(t, dir, "rollout-"+id+".jsonl", raw)
		}
		c := newCollectorCatalog(t, homes)
		calls := 0
		r := providerReader{harness: "codex", ref: agentapi.SourceRef{Path: path}, pendingRollouts: func() agentapi.CodexRolloutLookup { calls++; return c }}
		ordinaryError := agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic refusal"))
		err := r.observePendingHistory(t.Context(), ordinaryError)
		var gotOriginal, wantOriginal *agentapi.SourceError
		if !errors.As(err, &gotOriginal) || !errors.As(ordinaryError, &wantOriginal) || gotOriginal != wantOriginal || reflect.TypeOf(err) != reflect.TypeOf(ordinaryError) || calls != 0 {
			t.Fatal("ordinary source error constructed a catalog")
		}
		original := errors.Join(ordinaryError, archive.ErrRelatedHistory)
		err = r.observePendingHistory(t.Context(), original)
		var got, want *agentapi.SourceError
		if !errors.As(ordinaryError, &want) || !errors.As(err, &got) || got != want || !errors.Is(err, archive.ErrRelatedHistory) {
			t.Fatalf("original typed refusal lost: %v", err)
		}
		for _, private := range append(homes, id, "synthetic-private-body", "/synthetic/project") {
			if strings.Contains(err.Error(), private) {
				t.Fatal("pending diagnostic exposed private evidence")
			}
		}
		if !strings.Contains(err.Error(), "2 candidate rollout locators") {
			t.Fatalf("metadata-only diagnostic failed: %v %#v", err, c.sweeps)
		}
	})
}

func TestPendingCatalogCloseFailurePreservesPrivateRefusal(t *testing.T) {
	t.Parallel()
	lookup := &failingCloseCatalog{collectorCatalog: newCollectorCatalog(t, []string{t.TempDir()})}
	r := providerReader{harness: "codex", pendingRollouts: func() agentapi.CodexRolloutLookup { return lookup }}
	original := errors.Join(agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic refusal")), archive.ErrRelatedHistory)
	err := r.observePendingHistory(t.Context(), original)
	if lookup.closes != 1 || !errors.Is(err, original) || !strings.Contains(err.Error(), "locator evidence unavailable") || strings.Contains(err.Error(), "private-close-locator") {
		t.Fatalf("cleanup failure lost refusal or disclosed native evidence: %v", err)
	}
}

type failingCloseCatalog struct {
	*collectorCatalog
	closes int
}

func (c *failingCloseCatalog) BeginValidationSlice(ctx context.Context, limits agentapi.CodexValidationLimits) (agentapi.CodexRolloutSlice, error) {
	slice, err := c.collectorCatalog.BeginValidationSlice(ctx, limits)
	if err != nil {
		return nil, err
	}
	return failingCloseSlice{CodexRolloutSlice: slice, owner: c}, nil
}

type failingCloseSlice struct {
	agentapi.CodexRolloutSlice
	owner *failingCloseCatalog
}

func (s failingCloseSlice) Close() error {
	s.owner.closes++
	return errors.Join(s.CodexRolloutSlice.Close(), errors.New("private-close-locator"))
}
