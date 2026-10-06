package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/rolloutcatalog"
)

func TestCollectorCachesFailedSweepUntilSliceExpires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := rolloutcatalog.New([]string{t.TempDir()}, rolloutcatalog.Limits{CheckOperations: 1})
		set := &sourcePassSet{env: agentapi.SourceEnvironment{CodexRollouts: c}, passes: map[sourcePassKey]agentapi.SourcePass{}}
		provider := &sliceLifetimeProvider{}
		reader := providerReader{passes: set}
		for range 20 {
			_, release, err := reader.pass(t.Context(), provider, "codex")
			if release != nil {
				_ = release()
			}
			if agentapi.Failure(err) != agentapi.Limit {
				t.Fatalf("failed sweep did not stop reader: %v", err)
			}
		}
		if provider.opens != 0 || c.Counters().ValidationSweeps != 1 {
			t.Fatalf("failed sweep repeated or opened provider: opens=%d counters=%#v", provider.opens, c.Counters())
		}
		time.Sleep(30 * time.Second)
		if _, _, err := reader.pass(t.Context(), provider, "codex"); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal(err)
		}
		if c.Counters().ValidationSweeps != 2 {
			t.Fatal("expired failed slice did not receive a fresh sweep budget")
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, err := reader.pass(cancelled, provider, "codex"); !errors.Is(err, context.Canceled) {
			t.Fatal("cached failure bypassed cancellation", err)
		}
		if err := set.slice.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCollectorRenewsExpiredSliceOnlyAfterReaderCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := rolloutcatalog.New([]string{t.TempDir()}, rolloutcatalog.Limits{})
		set := &sourcePassSet{env: agentapi.SourceEnvironment{CodexRollouts: c}, passes: map[sourcePassKey]agentapi.SourcePass{}}
		provider := &sliceLifetimeProvider{}
		reader := providerReader{passes: set}
		pass, release, err := reader.pass(t.Context(), provider, "codex")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{}, agentapi.ReadLimits{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		if _, _, err := reader.pass(t.Context(), provider, "codex"); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal("renewed active snapshot", err)
		}
		if provider.closes != 0 || c.Counters().ValidationSweeps != 1 {
			t.Fatal("active reader closed or catalog reswept")
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		_, release, err = reader.pass(t.Context(), provider, "codex")
		if err != nil {
			t.Fatal(err)
		}
		if provider.closes != 1 || c.Counters().ValidationSweeps != 2 || provider.opens != 2 {
			t.Fatal("closed reader did not renew fairly")
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
		for _, pass := range set.passes {
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := set.slice.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOrdinaryCollectorPassDoesNotCreateValidationSlice(t *testing.T) {
	t.Parallel()
	provider := &sliceLifetimeProvider{}
	set := &sourcePassSet{env: agentapi.SourceEnvironment{}, passes: map[sourcePassKey]agentapi.SourcePass{}}
	pass, err := set.get(t.Context(), sourcePassKey{name: "codex"}, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	if set.slice != nil || provider.bound != nil {
		t.Fatal("ordinary capture acquired history evidence")
	}
}

type sliceLifetimeProvider struct {
	observationProvider
	opens  int
	closes int
	bound  agentapi.CodexRolloutLookup
}

func (p *sliceLifetimeProvider) OpenPass(_ context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.opens++
	p.bound = env.CodexRollouts
	return sliceLifetimePass{observationPass: observationPass{}, owner: p}, nil
}

type sliceLifetimePass struct {
	observationPass
	owner *sliceLifetimeProvider
}

func (p sliceLifetimePass) Close() error { p.owner.closes++; return nil }

func TestExplicitCatalogReadsHaveBoundedOverallContext(t *testing.T) {
	t.Parallel()
	c := rolloutcatalog.New([]string{t.TempDir()}, rolloutcatalog.Limits{})
	reader := providerReader{rollouts: c}
	bounded, cancel := reader.validationContext(context.Background())
	defer cancel()
	deadline, ok := bounded.Deadline()
	if !ok || time.Until(deadline) > 30*time.Second {
		t.Fatal("explicit read lacks overall bound")
	}
	ordinary := providerReader{}
	ctx, closeCtx := ordinary.validationContext(context.Background())
	defer closeCtx()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("ordinary read context changed")
	}
}

func TestExplicitCatalogDefaultReadDeadlineFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := rolloutcatalog.New([]string{t.TempDir()}, rolloutcatalog.Limits{})
		reader := providerReader{rollouts: c}
		bounded, cancel := reader.validationContext(context.Background())
		defer cancel()
		slice, err := c.BeginValidationSlice(bounded, agentapi.CodexValidationLimits{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := slice.Close(); err != nil {
				t.Error(err)
			}
		}()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if err := slice.Valid(bounded); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("default context ignored deadline", err)
		}
		if c.Counters().Headers != 0 {
			t.Fatal("expiry enumerated or admitted content")
		}
	})
}

func TestCollectorRealCodexProviderRenewsAcrossManyThreads(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const threads = 260
	paths := make([]string, 0, threads)
	for i := range threads {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic","timestamp":"2026-10-01T12:00:00Z","source":"cli","cli_version":"0.160.0","originator":"codex_cli_rs"}}` + "\n"
		paths = append(paths, writeTranscript(t, dir, "rollout-"+id+".jsonl", raw))
	}
	c := rolloutcatalog.New([]string{home}, rolloutcatalog.Limits{})
	opts := Options{Sources: testSources, CodexRollouts: c}
	closePasses := openCursorPass(nil, &opts)
	defer func() {
		if err := closePasses(); err != nil {
			t.Error(err)
		}
	}()
	for _, path := range paths {
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: path}
		reader, ok := newSourceReader(reg, opts)
		if !ok {
			t.Fatal("missing source reader")
		}
		if _, err := reader.Signature(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	counts := c.Counters()
	if counts.ValidationSweeps != 2 || counts.Headers != threads || counts.Checks != threads || counts.CheckOperations > 2*(threads+10) || opts.sourcePasses.active != 0 {
		t.Fatalf("real caller did not share and renew bounded sweeps: %#v", counts)
	}
}
