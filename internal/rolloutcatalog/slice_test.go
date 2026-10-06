package rolloutcatalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestValidationSlicesShareSweepsAtNativeStoreScale(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const threads = 1100
	for i := range threads {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		fixture(t, home, "sessions/2026/10", id, id, nil, "{\"ordinal\":1}\n")
		rid := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1)
		fixture(t, home, "archived_sessions/2026", rid, id, nil, "{\"ordinal\":1}\n")
	}
	ctx := t.Context()
	if raceEnabled {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		ctx = bounded
	}
	c := New([]string{home}, Limits{})
	var bound agentapi.CodexRolloutSlice
	var sweeps int
	for i := range threads {
		if i%256 == 0 {
			if bound != nil {
				_ = bound.Close()
			}
			var err error
			bound, err = c.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			sweeps++
		}
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		set, err := bound.Thread(t.Context(), id)
		if err != nil || !set.Complete || len(set.Candidates) != 2 {
			t.Fatalf("thread %d: %#v %v", i, set, err)
		}
		if err := bound.Check(t.Context(), id, set.Revision); err != nil {
			t.Fatal(err)
		}
	}
	_ = bound.Close()
	counts := c.Counters()
	if counts.ValidationSweeps != sweeps || counts.Headers != threads*2 || counts.Checks != threads || counts.NativeQueries != 0 || counts.CheckOperations > threads*2*sweeps+100 {
		t.Fatalf("unexpected work %#v", counts)
	}
	if counts.Stats < threads*2 || counts.Stats > threads*70 || counts.RootOpens > threads*60 || counts.FileOpens < threads*2 || counts.MetadataJoins > threads*5 || counts.FileBytes == 0 {
		t.Fatalf("actual work unbounded or unmeasured %#v", counts)
	}
	// The selected source still opens and checks the header on its own handle.
	before := c.Counters()
	e := c.files[0]
	file, err := transcriptio.Open(c.Files(), e.ref.Path, transcriptio.OpenPolicy{RejectSymlinks: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.CheckPrefix(t.Context(), e.headerLen, e.header); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Check(), file.Close()); err != nil {
		t.Fatal(err)
	}
	after := c.Counters()
	if after.FileOpens <= before.FileOpens || after.FileBytes <= before.FileBytes || after.Stats <= before.Stats {
		t.Fatalf("selected source work missing: %#v -> %#v", before, after)
	}
	t.Logf("2200 entries / 1100 threads / %d slices: %#v", sweeps, after)
}

func TestValidationSliceCancellationAndExhaustionAllowFairRenewal(t *testing.T) {
	t.Parallel()
	c := New([]string{t.TempDir()}, Limits{})
	bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{Steps: 1})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := bound.Rollout(cancelled, thread); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := bound.Rollout(t.Context(), thread); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Rollout(t.Context(), thread); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	_ = bound.Close()
	next, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := next.Valid(t.Context()); err != nil || c.invalid {
		t.Fatalf("renewal poisoned: %v", err)
	}
}

func TestValidationSliceFailedSweepCachedOnlyWithinSlice(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	fixture(t, home, "sessions", thread, thread, nil, "")
	c := New([]string{home}, Limits{CheckOperations: 1})
	bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	counts := c.Counters()
	for range 20 {
		if _, err := bound.Thread(t.Context(), thread); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal(err)
		}
	}
	if c.Counters() != counts || c.invalid {
		t.Fatal("failed slice rescanned or poisoned epoch")
	}
	_ = bound.Close()
	next, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	if c.Counters().ValidationSweeps != 2 || agentapi.Failure(next.Valid(t.Context())) != agentapi.Limit {
		t.Fatal("renewal did not get fresh validation budget")
	}
}

func TestValidationSliceExpiresAfterCompletedSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New([]string{t.TempDir()}, Limits{})
		bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := bound.Close(); err != nil {
				t.Error(err)
			}
		}()
		time.Sleep(30 * time.Second)
		if err := bound.Valid(t.Context()); agentapi.Failure(err) != agentapi.Limit {
			t.Fatal(err)
		}
		if c.invalid {
			t.Fatal("expiry poisoned epoch")
		}
	})
}

func TestRenewedSliceAndLegacyCheckObserveMembershipChanges(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	fixture(t, home, "sessions/nested", thread, thread, nil, "")
	c := New([]string{home}, Limits{})
	bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := bound.Thread(t.Context(), thread)
	if err != nil {
		t.Fatal(err)
	}
	fixture(t, home, "sessions/nested", revision, thread, nil, "")
	// An active slice is one bounded caller operation; it cannot detect unseen
	// membership on each cheap lookup. Legacy Check still observes changes freshly.
	if err := bound.Check(t.Context(), thread, set.Revision); err != nil {
		t.Fatal(err)
	}
	if err := c.Check(t.Context(), thread, set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal(err)
	}
	if err := bound.Valid(t.Context()); err == nil {
		t.Fatal("invalid epoch accepted by bound lookup")
	}
	_ = bound.Close()
	renewed, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := renewed.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := renewed.Valid(t.Context()); err == nil {
		t.Fatal("changed epoch reused")
	}
}

func TestRenewedSliceRevalidatesHeaderAndNativeCurrentEvidence(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		t.Run(strconv.FormatBool(native), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			fixture(t, home, "sessions", thread, thread, nil, "")
			c := New([]string{home}, Limits{})
			bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			_ = bound.Close()
			if native {
				if err := os.WriteFile(home+"/state_5.sqlite", []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture(t, home, "sessions", thread, thread, map[string]any{"cwd": "/changed"}, "")
			}
			renewed, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := renewed.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := renewed.Valid(t.Context()); err == nil {
				t.Fatal("changed header/current accepted")
			}
		})
	}
}

func TestRenewedSliceRejectsApprovedHomeReplacement(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	home := parent + "/approved"
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	fixture(t, home, "sessions", thread, thread, nil, "")
	c := New([]string{home}, Limits{})
	bound, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_ = bound.Close()
	if err := os.Rename(home, home+"-old"); err != nil {
		t.Fatal(err)
	}
	fixture(t, home, "sessions", thread, thread, nil, "")
	next, err := c.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := next.Valid(t.Context()); err == nil || !c.invalid {
		t.Fatal("replacement reused existing proof")
	}
}
