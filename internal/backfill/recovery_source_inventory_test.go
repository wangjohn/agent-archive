package backfill

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoverySourceInventoryRenewsWithoutBodyReads(t *testing.T) {
	tr := newTree(t)
	dir := tr.mkdir("home/store")
	file := tr.write("home/store/source.jsonl", "header\n")
	env := tr.env()
	calls := 0
	env.Lstat = func(path string) (fs.FileInfo, error) { calls++; return os.Lstat(path) }
	inv := newRecoverySourceInventory(env)
	seen := inv.environment()
	if _, err := seen.ReadDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := seen.Lstat(file); err != nil {
		t.Fatal(err)
	}
	inv.env.Open = func(string) (io.ReadCloser, error) { t.Fatal("renewal reopened content"); return nil, nil }
	for range 4 {
		before := calls
		if !inv.current(t.Context()) || calls-before != 2 {
			t.Fatal("renewal did not use exactly its observed paths", calls-before)
		}
	}
	tr.write("home/store/source.jsonl", "new larger header\n")
	if inv.current(t.Context()) {
		t.Fatal("changed non-witness header retained membership")
	}
}

func TestRecoverySourceInventoryBoundsAndHonorsCancellation(t *testing.T) {
	tr := newTree(t)
	inv := newRecoverySourceInventory(tr.env())
	for n := range recoverySourceObservationLimit + 1 {
		inv.observe(filepath.Join(tr.home, strconv.Itoa(n)))
	}
	if len(inv.stamps) != recoverySourceObservationLimit || inv.current(t.Context()) {
		t.Fatal("incomplete bounded inventory authorized recovery")
	}
	inv = newRecoverySourceInventory(tr.env())
	inv.observe(filepath.Join(tr.home, "absent-store"))
	if !inv.current(t.Context()) {
		t.Fatal("unchanged absent store rejected")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	inv.env.Lstat = func(string) (fs.FileInfo, error) { t.Fatal("probed after cancellation"); return nil, nil }
	if inv.current(ctx) {
		t.Fatal("cancelled inventory accepted")
	}
}

func TestFirstRunRecoveryAbsentStoreAppearanceRequiresNewPlan(t *testing.T) {
	tr, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
	clone := tr.repo("home/clone")
	tr.write(filepath.Join("home", claudeFile("clone", "new-clone")), claudeTranscript("new-clone", clone, fixedNow))
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("newly populated native store escaped renewal")
	}
}

// More than 1,024 sessions in a single repository must not consume the separate
// 1,024 Git-root allowance. Confirmation and admission share one membership
// sweep per slice, even when many deleted sessions refer to that repository.
func TestFirstRunRecoveryLargeMembershipSharesCallerSlices(t *testing.T) {
	tr, env, cfg, root, _ := firstRunRecoveryFixture(t, false)
	const liveFiles = 1100
	for n := range liveFiles {
		id := fmt.Sprintf("00000000-0000-0000-0001-%012d", n)
		tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, root, fixedNow.Add(-48*time.Hour)))
	}
	const deletedFiles = 12
	for n := range deletedFiles - 1 {
		id := fmt.Sprintf("00000000-0000-0000-0002-%012d", n)
		gone := tr.path(fmt.Sprintf("home/.codex/worktrees/deleted-%d/repo", n))
		body := strings.Replace(codexTranscript(id, id, gone, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`, 1)
		tr.write(filepath.Join("home", codexFile(id)), body)
	}
	var stats atomic.Int64
	var opens atomic.Int64
	var identities atomic.Int64
	var sweeps atomic.Int64
	var nativeReads atomic.Int64
	env.Sources = membershipSources{SourcesLookup: env.Sources, ImportsLookup: testSources, NativeHeadersLookup: testSources, reads: &nativeReads}
	env.Lstat = func(path string) (fs.FileInfo, error) {
		stats.Add(1)
		if path == filepath.Join(env.Home, ".codex", "sessions") {
			sweeps.Add(1)
		}
		return os.Lstat(path)
	}
	env.Open = func(path string) (io.ReadCloser, error) { opens.Add(1); return os.Open(path) }
	lookup := env.RepositoryIdentity
	env.RepositoryIdentity = func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
		identities.Add(1)
		return lookup(ctx, path)
	}
	p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, Since: fixedNow.Add(-24 * time.Hour).Format(dateLayout)})
	if len(p.Imported()) != deletedFiles+1 {
		t.Fatal("large inventory did not plan selected sessions", p.Imported())
	}
	for range 2 {
		before, bodyReads, gitReads, sourceReads := stats.Load(), opens.Load(), identities.Load(), nativeReads.Load()
		if err := p.CheckRecovery(t.Context()); err != nil {
			t.Fatal(err)
		}
		cost := stats.Load() - before
		if cost < liveFiles || cost > 2*liveFiles+200 || opens.Load() != bodyReads || identities.Load() != gitReads || nativeReads.Load() != sourceReads {
			t.Fatal("confirmation repeated membership/bodies/Git per session", cost, opens.Load()-bodyReads, identities.Load()-gitReads)
		}
		t.Logf("confirmation slice: %d native stats, %d body opens, %d Git observations", cost, opens.Load()-bodyReads, identities.Load()-gitReads)
	}
	if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	before, bodyReads, priorSweeps, sourceReads := stats.Load(), opens.Load(), sweeps.Load(), nativeReads.Load()
	result, err := (Registration{Context: t.Context(), Sources: env.Sources, Home: home, Store: store, AdmittedAt: fixedNow}).Run(p.Imported())
	if err != nil || len(result.Sessions) != deletedFiles+1 {
		t.Fatal(result, err)
	}
	// Count the actual slices: the existing wall-clock hold limit may split
	// reservation and admission, each of which gets one fresh membership sweep.
	// Durable materialization needs exactly one bounded source read per parent;
	// recovery renewal must not add any further body reads.
	cost := stats.Load() - before
	sliceCount := sweeps.Load() - priorSweeps
	if sliceCount < 1 || sliceCount > 2*(deletedFiles+1) || cost < liveFiles*sliceCount || cost > (2*liveFiles+200)*sliceCount || opens.Load() != bodyReads || nativeReads.Load()-sourceReads != int64(deletedFiles+1) {
		t.Fatal("admission repeated transcript reads or per-session membership", cost, opens.Load()-bodyReads)
	}
	t.Logf("admission: %d slices, %d native stats, %d body opens", sliceCount, cost, opens.Load()-bodyReads)
}

func TestFirstRunRecoveryMembershipCancellationStopsCallerSweep(t *testing.T) {
	_, env, cfg, _, _ := firstRunRecoveryFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checking, afterCancel := false, 0
	env.Lstat = func(path string) (fs.FileInfo, error) {
		if checking {
			if ctx.Err() != nil {
				afterCancel++
			}
			if path == filepath.Join(env.Home, ".codex", "sessions") {
				cancel()
			}
		}
		return os.Lstat(path)
	}
	p := plan(t, env, nil, cfg, Filters{})
	checking = true
	if err := p.CheckRecovery(ctx); err == nil || ctx.Err() == nil || afterCancel != 0 {
		t.Fatal("cancelled caller continued membership sweep", err, afterCancel)
	}
}

type membershipDirectory struct {
	fs.DirEntry
	name string
}

func (d membershipDirectory) Name() string { return d.name }

func TestFirstRunRecoveryMembershipOverflowKeepsCallerPending(t *testing.T) {
	tr, env, cfg, _, goneID := firstRunRecoveryFixture(t, false)
	dir := tr.mkdir("home/template-directory")
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var template fs.DirEntry
	for _, entry := range entries {
		if entry.Name() == filepath.Base(dir) {
			template = entry
		}
	}
	if template == nil {
		t.Fatal("missing synthetic directory")
	}
	store := filepath.Join(env.Home, ".codex", "sessions")
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		entries, err := os.ReadDir(path)
		if path == store {
			for n := range recoverySourceObservationLimit {
				entries = append(entries, membershipDirectory{DirEntry: template, name: fmt.Sprintf("absent-%05d", n)})
			}
		}
		return entries, err
	}
	p := plan(t, env, nil, cfg, Filters{})
	if c := candidate(t, p, goneID); c.Skip != SkipWorktreeUnresolved || c.ProjectResolution != nil {
		t.Fatal("overflow authorized incomplete membership", c)
	}
}

// The spy delegates every native operation to the production provider; it only
// counts full reads so renewal cannot hide a reread behind the source API.
type membershipSources struct {
	agentapi.ImportsLookup
	agentapi.NativeHeadersLookup
	agentapi.SourcesLookup
	reads *atomic.Int64
}

func (s membershipSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.SourcesLookup.LookupSources(name)
	if !ok {
		return nil, filter, false
	}
	return membershipProvider{SourceProvider: provider, reads: s.reads}, filter, true
}

type membershipProvider struct {
	agentapi.SourceProvider
	reads *atomic.Int64
}

func (p membershipProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return membershipPass{SourcePass: pass, reads: p.reads}, nil
}

func (p membershipProvider) OpenAdmissionPass(ctx context.Context, env agentapi.SourceEnvironment, ref agentapi.SourceRef) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.(agentapi.AdmissionSourceProvider).OpenAdmissionPass(ctx, env, ref)
	if err != nil {
		return nil, err
	}
	return membershipPass{SourcePass: pass, reads: p.reads}, nil
}

type membershipPass struct {
	agentapi.SourcePass
	reads *atomic.Int64
}

func (p membershipPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	p.reads.Add(1)
	return p.SourcePass.Read(ctx, ref, limits)
}
