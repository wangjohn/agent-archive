package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A fake source adapter deliberately reports usable evidence; only shared
// policy may authorize its project, timestamp, identity and destination.
type factAdapter struct {
	codexAdapter
	alter func(*Candidate)
}

func (a factAdapter) Inspect(ctx context.Context, source SourceDescriptor) Observation {
	o := a.codexAdapter.Inspect(ctx, source)
	if a.alter != nil && o.Outcome == outcomeUsable {
		a.alter(&o.Candidate)
	}
	return o
}

func (factAdapter) Supported(Candidate) bool { return true }

type candidateCase string

const (
	candidateExcluded        candidateCase = "excluded"
	candidateOldStart        candidateCase = "old_start"
	candidateDifferentAgent  candidateCase = "different_agent"
	candidateDifferentSource candidateCase = "different_source"
	candidateInherited       candidateCase = "inherited"
	candidateUnknownEvidence candidateCase = "unknown_evidence"
	candidateFutureTask      candidateCase = "future_task"
)

func TestAdapterFactsCannotGrantAdmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []candidateCase{candidateExcluded, candidateOldStart, candidateDifferentAgent, candidateDifferentSource, candidateInherited, candidateUnknownEvidence, candidateFutureTask} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			native := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
			excluded := filepath.Join(project, "excluded")
			if mode == candidateExcluded {
				if err := os.MkdirAll(excluded, 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: excluded, Included: false})
				prior := cfg
				if err := config.ReconcileDiscovery(&cfg, prior, at); err != nil {
					t.Fatal(err)
				}
				if err := config.Save(store.Home(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			adapter := factAdapter{alter: func(c *Candidate) {
				switch mode {
				case candidateExcluded:
					c.WorkingDirectory = excluded
				case candidateOldStart:
					c.StartedAt = at.Add(-time.Hour)
				case candidateDifferentAgent:
					c.Agent = "claude"
				case candidateDifferentSource:
					c.Source.Locator = filepath.Join(t.TempDir(), "other")
				case candidateInherited:
					c.ParentNativeID = "unadmitted-parent"
				case candidateFutureTask:
					c.FirstTaskAt = at.Add(24 * time.Hour)
				case candidateUnknownEvidence:
					c.StartEvidence = "mtime"
				}
			}}
			h, err := runWithAdapters(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, []SourceAdapter{adapter})
			if err != nil || h.Registered != 0 {
				t.Fatalf("adapter bypassed shared policy: %#v %v", h, err)
			}
			if _, found, err := store.ArchiveSessionID(sessionKey("codex", native)); err != nil || found {
				t.Fatal("rejected facts allocated identity", err)
			}
		})
	}
}

func TestOnlyCodexIsRegisteredAndEnumerationIsBoundedAndCancelled(t *testing.T) {
	t.Parallel()
	adapters := registeredAdapters()
	if len(adapters) != 1 || findAdapter(adapters, "codex") == nil || findAdapter(adapters, "claude") != nil || findAdapter(adapters, "cursor") != nil {
		t.Fatal("discovery broadened to another agent")
	}
	_, cfg, at, root := fixture(t)
	for n := 1; n <= 600; n++ {
		writeRollout(t, root, cfg.Archive.Projects[0].Root, at, n, "sessions")
	}
	a := findAdapter(adapters, "codex")
	cookie := int64(0)
	seen := 0
	for range 100 {
		batch, err := a.Enumerate(context.Background(), root, "sessions", cookie)
		if err != nil || len(batch.Entries) > 256 {
			t.Fatal("unbounded enumeration", err)
		}
		seen += len(batch.Entries)
		cookie = batch.Continuation
		if batch.Complete {
			break
		}
	}
	if seen != 600 {
		t.Fatalf("continuation lost coverage: %d", seen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Enumerate(ctx, root, "sessions", 0); err == nil {
		t.Fatal("enumeration ignored cancellation")
	}
}

// A previous native observation is metadata, not durable producer permission.
// Retiring a producer must reject its cached usable candidate on the next pass.
func TestCachedUsableFactsRecheckCurrentProducerSupport(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	source := codexAdapter{}.Describe(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	observation := codexAdapter{supported: syntheticSupport}.Inspect(context.Background(), source.Source)
	if observation.Outcome != outcomeUsable {
		t.Fatal("fixture lacks usable cached facts")
	}
	now := at.Add(2 * time.Minute)
	prior := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{source.Source.Locator: {Size: source.Fingerprint.Size, Mtime: source.Fingerprint.Mtime, Checked: now, Observation: observation}}, Health: Health{Supported: true}}
	if err := local.Write(filepath.Join(store.Home(), "discovery-catalog.json"), prior); err != nil {
		t.Fatal(err)
	}
	h, err := Run(context.Background(), store, cfg, Options{Now: func() time.Time { return now }})
	if err != nil || h.Probes != 0 || h.Supported || h.Registered != 0 || h.Outcomes["unsupported_producer"] == 0 {
		t.Fatalf("cached facts bypassed current producer gate: %#v %v", h, err)
	}
	if _, found, err := store.ArchiveSessionID(sessionKey("codex", native)); err != nil || found {
		t.Fatal("unsupported cache allocated identity", err)
	}
}

func TestDelayedFirstTaskUsesNativeSessionStartForConsent(t *testing.T) {
	t.Parallel()
	for _, oldStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized_start", true: "before_consent"}[oldStart], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			started := at.Add(time.Minute)
			if oldStart {
				started = at.Add(-time.Hour)
			}
			writeRollout(t, root, cfg.Archive.Projects[0].Root, started, 1, "sessions")
			adapter := factAdapter{alter: func(c *Candidate) { c.FirstTaskAt = at.Add(24 * time.Hour) }}
			h, err := runWithCensus(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(24*time.Hour + time.Minute) }}, []SourceAdapter{adapter})
			want := 1
			if oldStart {
				want = 0
			}
			if err != nil || h.Registered != want {
				t.Fatalf("idle/consent mismatch: %#v %v", h, err)
			}
		})
	}
}

// run's contract seam is private: only synthetic tests may inject support.
func run(ctx context.Context, store *state.Store, cfg config.Config, o Options, supported func(sourcefacts.CodexMeta) bool) (Health, error) {
	return runWithCensus(ctx, store, cfg, o, []SourceAdapter{codexAdapter{supported: supported}})
}

// runWithCensus models successive scheduled passes: discovery requests missing
// identities, the collector census establishes ownership/absence, and the next
// scan may admit. Production has no adapter override or test-only admission.
func runWithCensus(ctx context.Context, store *state.Store, cfg config.Config, o Options, adapters []SourceAdapter) (Health, error) {
	h, err := runWithAdapters(ctx, store, cfg, o, adapters)
	if err != nil || h.Outcomes["admission_retry"] == 0 {
		return h, err
	}
	if err := store.RecoverSessionIndexIfNeeded(ctx); err != nil {
		return h, err
	}
	return runWithAdapters(ctx, store, cfg, o, adapters)
}
