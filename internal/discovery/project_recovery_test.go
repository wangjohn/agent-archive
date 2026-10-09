package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeletedWorktreeDiscoveryRetainsCreationConsent(t *testing.T) {
	for _, recent := range []bool{false, true} {
		t.Run(map[bool]string{true: "new", false: "old"}[recent], func(t *testing.T) {
			store, cfg, at, root := fixture(t)
			gone := filepath.Join(t.TempDir(), "worktrees", "gone", "repo")
			start := at.Add(-time.Hour)
			if recent {
				start = at.Add(time.Minute)
			}
			id := writeRollout(t, root, gone, start, 1, "sessions")
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var records []json.RawMessage
			for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
				records = append(records, json.RawMessage(line))
			}
			var first map[string]json.RawMessage
			if err = json.Unmarshal(records[0], &first); err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(first["payload"], &payload); err != nil {
				t.Fatal(err)
			}
			payload["git"] = json.RawMessage(`{"repository_url":"git@example.test:acme/repo.git"}`)
			first["payload"], _ = json.Marshal(payload)
			records[0], _ = json.Marshal(first)
			var out []byte
			for _, record := range records {
				out = append(out, record...)
				out = append(out, '\n')
			}
			if err = os.WriteFile(path, out, 0600); err != nil {
				t.Fatal(err)
			}
			key := archive.RepoKey("https://example.test/acme/repo")
			calls := 0
			opts := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }, RepositoryIdentity: func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
				calls++
				return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
			}}
			h, err := run(context.Background(), store, cfg, opts, syntheticSupport)
			want := 0
			if recent {
				want = 1
			}
			if err != nil || h.Registered != want {
				t.Fatalf("%+v %v", h, err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != want {
				t.Fatal(regs, err)
			}
			if recent && (regs[0].ProjectResolution == nil || regs[0].ProjectResolution.OriginalCwd != gone || regs[0].ProjectRoot != cfg.Archive.Projects[0].Root || regs[0].RepoKey != key) {
				t.Fatal(regs)
			}
			if recent {
				published, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
				if err != nil || len(published.Published) != 1 {
					t.Fatalf("recovered source publication/readback: %+v %v", published, err)
				}
			}
			wantCalls := 1
			if recent {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("lookup count %d", calls)
			}
		})
	}
}

func TestDeletedWorktreeImportContinuesWithoutRemapping(t *testing.T) {
	store, cfg, at, root := fixture(t)
	gone := filepath.Join(t.TempDir(), "gone")
	native := writeRollout(t, root, gone, at.Add(-time.Hour), 7, "sessions")
	project := cfg.Archive.Projects[0].Root
	original, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: native}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: native, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, Origin: archive.SessionOriginImport, AdmittedAt: at.Add(time.Minute), SessionStartedAt: at.Add(-time.Hour), ProjectResolution: &archive.ProjectResolution{OriginalCwd: gone, Root: project, Method: "explicit_mapping", Context: "private-import-context"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(3 * time.Minute) }, RepositoryIdentity: func(context.Context, string) sourcefacts.RepositoryIdentity {
		calls++
		return sourcefacts.RepositoryIdentity{}
	}}
	h, err := run(context.Background(), store, cfg, opts, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["admission_retry"] != 0 || calls != 0 {
		t.Fatalf("%+v %v calls%d", h, err, calls)
	}
	after, found, err := store.LoadRegistration(original.ArchiveSessionID)
	if err != nil || !found || after.ProjectRoot != project || after.Origin != archive.SessionOriginImport || !after.AdmittedAt.Equal(original.AdmittedAt) || after.ProjectResolution.Context != original.ProjectResolution.Context {
		t.Fatal(after, err)
	}
	if err := store.RecordRemoval("codex", native, state.RemovalReasonUndo, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err = run(context.Background(), store, cfg, opts, syntheticSupport)
	if err != nil || h.Registered != 0 {
		t.Fatal(h, err)
	}
}

func TestProjectRecoveryMetadataBudgetStaysRetryable(t *testing.T) {
	resolver := sourcefacts.NewProjectResolver()
	resolver.Operations = 1024
	s := scan{resolver: resolver, recovery: sourcefacts.NewRecoveryResolver(nil, nil, filepath.Clean, nil, nil)}
	_, outcome, attempted := s.recoverProject(Candidate{WorkingDirectory: filepath.Join(t.TempDir(), "gone")})
	if !attempted || outcome != sourcefacts.RecoveryBudgetExhausted {
		t.Fatal(outcome, attempted)
	}
}

func TestRecoveredDiscoveryRechecksEvidenceBeforeAdmission(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		store, cfg, at, root := fixture(t)
		gone := filepath.Join(t.TempDir(), "gone")
		native := writeRollout(t, root, gone, at.Add(time.Minute), 33, "sessions")
		path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.Replace(raw, []byte(`"source":"cli"`), []byte(`"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`), 1)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		key := archive.RepoKey("https://example.test/acme/repo")
		opts := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }, RepositoryIdentity: func(_ context.Context, rootPath string) sourcefacts.RepositoryIdentity {
			if changeSource {
				if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return sourcefacts.RepositoryIdentity{Root: rootPath, Key: key, Known: true}
		}, RepositoryIdentityCurrent: func(sourcefacts.RepositoryIdentity) bool { return changeSource }}
		h, err := run(t.Context(), store, cfg, opts, syntheticSupport)
		if err != nil || h.Registered != 0 || (h.Outcomes[string(sourcefacts.RecoveryInventoryUnavailable)] == 0 && h.Outcomes[string(outcomeChanged)] == 0) {
			t.Fatal(h, err)
		}
	}
}

// changedHeaderAdapter mutates only after the real bounded header probe returns.
type changedHeaderAdapter struct {
	codexAdapter
	after func(SourceDescriptor)
}

func (a changedHeaderAdapter) Inspect(ctx context.Context, source SourceDescriptor) Observation {
	observation := a.codexAdapter.Inspect(ctx, source)
	a.after(source)
	return observation
}

type sourceChangePhase string

const (
	sourceChangeHeader         sourceChangePhase = "header"
	sourceChangeLookup         sourceChangePhase = "repository_lookup"
	sourceChangeRevalidation   sourceChangePhase = "repository_revalidation"
	sourceChangePersistedCache sourceChangePhase = "persisted_cache"
)

type sourceChangeMutation string

const (
	sourceChangeCwd         sourceChangeMutation = "excluded_cwd"
	sourceChangeRepoKey     sourceChangeMutation = "repository_key"
	sourceChangeProducer    sourceChangeMutation = "producer"
	sourceChangeCreation    sourceChangeMutation = "creation_time"
	sourceChangeReplacement sourceChangeMutation = "same_stamp_replacement"
)

func TestDiscoveryRejectsSourceChangesAcrossHeaderAndRecovery(t *testing.T) {
	for _, phase := range []sourceChangePhase{sourceChangeHeader, sourceChangeLookup, sourceChangeRevalidation, sourceChangePersistedCache} {
		for _, mutation := range []sourceChangeMutation{sourceChangeCwd, sourceChangeRepoKey, sourceChangeProducer, sourceChangeCreation, sourceChangeReplacement} {
			t.Run(string(phase)+"/"+string(mutation), func(t *testing.T) {
				store, cfg, at, root := fixture(t)
				gone := filepath.Join(t.TempDir(), "gone")
				excluded := filepath.Join(filepath.Dir(gone), "deny")
				if err := os.Mkdir(excluded, 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: excluded, ProjectID: archive.ProjectID(excluded), Included: false})
				if err := config.Save(store.Home(), cfg); err != nil {
					t.Fatal(err)
				}
				native := writeRollout(t, root, gone, at.Add(time.Minute), 44, "sessions")
				stable := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 45, "sessions")
				for _, id := range []string{native, stable} {
					if err := store.RequestSessionIndexRecovery(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: id}); err != nil {
						t.Fatal(err)
					}
				}
				if err := recoverytest.Exhaust(t.Context(), store, state.SessionIndexRecoverySlice, false); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte(`"source":"cli"`), []byte(`"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`), 1)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				original, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := false
				mutate := func() {
					if changed {
						return
					}
					changed = true
					newer := raw
					switch mutation {
					case sourceChangeCwd, sourceChangeReplacement:
						newer = bytes.Replace(raw, []byte(gone), []byte(excluded), 1)
					case sourceChangeRepoKey:
						newer = bytes.Replace(raw, []byte("acme/repo"), []byte("acme/nope"), 1)
					case sourceChangeProducer:
						newer = bytes.Replace(raw, []byte(`"source":"cli"`), []byte(`"source":"xyz"`), 1)
					case sourceChangeCreation:
						newer = bytes.ReplaceAll(raw, []byte(at.Add(time.Minute).Format(time.RFC3339Nano)), []byte(at.Add(-time.Hour).Format(time.RFC3339Nano)))
					}
					if bytes.Equal(raw, newer) {
						t.Fatal("mutation did not change the fixture")
					}
					target := path
					if mutation == sourceChangeReplacement {
						target += ".replacement"
					}
					if err := os.WriteFile(target, newer, 0600); err != nil {
						t.Fatal(err)
					}
					if mutation == sourceChangeReplacement {
						if err := os.Chtimes(target, original.ModTime(), original.ModTime()); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(target, path); err != nil {
							t.Fatal(err)
						}
						current, err := os.Stat(path)
						if err != nil || os.SameFile(original, current) || current.Size() != original.Size() || !current.ModTime().Equal(original.ModTime()) {
							t.Fatal("invalid replacement observation", err)
						}
					} else {
						// Guarantee a changed stamp even on a coarse-clock filesystem.
						stamp := original.ModTime().Add(time.Second)
						if err := os.Chtimes(path, stamp, stamp); err != nil {
							t.Fatal(err)
						}
					}
				}
				adapter := codexAdapter{supported: syntheticSupport}
				var sourceAdapter SourceAdapter = adapter
				if phase == sourceChangeHeader {
					sourceAdapter = changedHeaderAdapter{codexAdapter: adapter, after: func(source SourceDescriptor) {
						if source.Locator == path {
							mutate()
						}
					}}
				}
				if phase == sourceChangePersistedCache {
					entry := adapter.Describe(root, "sessions", filepath.Base(path))
					observation := adapter.Inspect(t.Context(), entry.Source)
					prior := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{path: {Size: entry.Fingerprint.Size, Mtime: entry.Fingerprint.Mtime, Checked: at.Add(2 * time.Minute), Observation: observation}}}
					if err := local.Write(filepath.Join(store.Home(), "discovery-catalog.json"), prior); err != nil {
						t.Fatal(err)
					}
					mutate()
				}
				key := archive.RepoKey("https://example.test/acme/repo")
				opts := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }, RepositoryIdentity: func(_ context.Context, rootPath string) sourcefacts.RepositoryIdentity {
					if phase == sourceChangeLookup {
						mutate()
					}
					rootKey := key
					if rootPath == excluded {
						rootKey = archive.RepoKey("https://example.test/acme/deny")
					}
					return sourcefacts.RepositoryIdentity{Root: rootPath, Key: rootKey, Known: true}
				}, RepositoryIdentityCurrent: func(sourcefacts.RepositoryIdentity) bool {
					if phase == sourceChangeRevalidation {
						mutate()
					}
					return true
				}}
				h, err := runWithAdapters(t.Context(), store, cfg, opts, []SourceAdapter{sourceAdapter})
				if err != nil || !changed || h.Registered != 1 {
					t.Fatalf("unrelated progress: %+v changed=%v err=%v", h, changed, err)
				}
				if phase != sourceChangePersistedCache && h.Outcomes[string(outcomeChanged)] == 0 {
					t.Fatalf("changed source was not retried: %+v", h)
				}
				regs, err := store.LoadRegistrations()
				if err != nil || len(regs) != 1 || regs[0].NativeSessionID != stable {
					t.Fatalf("stale ownership: %+v %v", regs, err)
				}
				h, err = runWithAdapters(t.Context(), store, cfg, opts, []SourceAdapter{adapter})
				if err != nil || h.Registered != 0 || h.Outcomes[string(outcomeChanged)] != 0 {
					t.Fatalf("settled retry: %+v %v", h, err)
				}
				if (mutation == sourceChangeCwd || mutation == sourceChangeReplacement) && h.Outcomes["excluded_project"] == 0 && h.Outcomes["project_not_authorized"] == 0 {
					t.Fatalf("current exclusion lost: %+v", h)
				}
			})
		}
	}
}

func TestCachedRecoveryObservationRejectsSameStampReplacement(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "reprobe", true: "probe_budget"}[exhausted], func(t *testing.T) {
			store, cfg, at, root := fixture(t)
			gone := filepath.Join(t.TempDir(), "gone")
			excluded := filepath.Join(filepath.Dir(gone), "deny")
			if err := os.Mkdir(excluded, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: excluded, ProjectID: archive.ProjectID(excluded), Included: false})
			native := writeRollout(t, root, gone, at.Add(time.Minute), 46, "sessions")
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
			adapter := codexAdapter{supported: syntheticSupport}
			entry := adapter.Describe(root, "sessions", filepath.Base(path))
			observation := adapter.Inspect(t.Context(), entry.Source)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, bytes.Replace(raw, []byte(gone), []byte(excluded), 1), 0600); err != nil {
				t.Fatal(err)
			}
			stamp := observation.SourceInfo.ModTime()
			if err := os.Chtimes(replacement, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			current, err := os.Stat(path)
			if err != nil || os.SameFile(observation.SourceInfo, current) || current.Size() != observation.SourceInfo.Size() || !current.ModTime().Equal(stamp) {
				t.Fatal("invalid same-stamp replacement", err)
			}
			c := catalog{Cache: map[string]cached{path: {Size: entry.Fingerprint.Size, Mtime: entry.Fingerprint.Mtime, Checked: at.Add(2 * time.Minute), Observation: observation}}}
			probes := 0
			if exhausted {
				probes = HeaderProbes
			}
			h := Health{Outcomes: map[string]int{}, Probes: probes}
			s := scan{resolver: sourcefacts.NewProjectResolver(), store: store, cfg: cfg, catalog: &c, health: &h, now: at.Add(2 * time.Minute), adapter: adapter, ctx: t.Context()}
			retry, stop := s.visitEntry(directory{Root: root, Path: "sessions"}, entry)
			if exhausted {
				if !retry || !stop || h.Probes != HeaderProbes || len(c.Cache) != 0 {
					t.Fatalf("exhausted probe admitted or retained stale facts: %+v retry=%v stop=%v", h, retry, stop)
				}
			} else if retry || stop || h.Probes != 1 || h.Outcomes["project_not_authorized"] != 1 {
				t.Fatalf("replacement did not reprobe current exclusion: %+v", h)
			}
			if h.Registered != 0 {
				t.Fatal("cached replacement granted ownership")
			}
		})
	}
}

func TestPersistedMissingConfiguredCwdReprobesWithoutRepositoryKey(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(map[bool]string{false: "reprobe", true: "probe_budget"}[exhausted], func(t *testing.T) {
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			gone, deny := filepath.Join(project, "gone"), filepath.Join(project, "deny")
			if err := os.Mkdir(deny, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: deny, ProjectID: archive.ProjectID(deny), Included: false})
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			native := writeRollout(t, root, gone, at.Add(time.Minute), 47, "sessions")
			if err := store.RequestSessionIndexRecovery(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: native}); err != nil {
				t.Fatal(err)
			}
			if err := recoverytest.Exhaust(t.Context(), store, state.SessionIndexRecoverySlice, false); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
			adapter := codexAdapter{supported: syntheticSupport}
			entry := adapter.Describe(root, "sessions", filepath.Base(path))
			observation := adapter.Inspect(t.Context(), entry.Source)
			if observation.Outcome != outcomeUsable || observation.Candidate.RecordedRepoKey != "" {
				t.Fatalf("bad original %+v", observation)
			}
			prior := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{path: {Size: entry.Fingerprint.Size, Mtime: entry.Fingerprint.Mtime, Checked: at.Add(2 * time.Minute), Observation: observation}}}
			if err := local.Write(filepath.Join(store.Home(), "discovery-catalog.json"), prior); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := path + ".replacement"
			if err := os.WriteFile(replacement, bytes.Replace(raw, []byte(gone), []byte(deny), 1), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(replacement, observation.SourceInfo.ModTime(), observation.SourceInfo.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			current, err := os.Stat(path)
			if err != nil || os.SameFile(observation.SourceInfo, current) || current.Size() != observation.SourceInfo.Size() || !current.ModTime().Equal(observation.SourceInfo.ModTime()) {
				t.Fatal("bad replacement", err)
			}
			settled := observation
			settled.SourceInfo = nil
			settled.Candidate.WorkingDirectory = project
			if cachedObservationNeedsProbe(settled, entry.Source) {
				t.Fatal("present-cwd persisted compatibility unexpectedly needs a probe")
			}
			if exhausted {
				var restored catalog
				if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &restored); err != nil {
					t.Fatal(err)
				}
				h := Health{Outcomes: map[string]int{}, Probes: HeaderProbes}
				scan := scan{resolver: sourcefacts.NewProjectResolver(), store: store, cfg: cfg, catalog: &restored, health: &h, now: at.Add(2 * time.Minute), adapter: adapter, ctx: t.Context()}
				retry, stop := scan.visitEntry(directory{Root: root, Path: "sessions"}, entry)
				if !retry || !stop || h.Registered != 0 || h.Probes != HeaderProbes || len(restored.Cache) != 0 {
					t.Fatalf("probe exhaustion reused stale missing-cwd facts: %+v retry=%v stop=%v", h, retry, stop)
				}
			}
			h, err := runWithAdapters(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, []SourceAdapter{adapter})
			if err != nil {
				t.Fatal(err)
			}
			if h.Registered != 0 || h.Probes != 1 || h.Outcomes["project_not_authorized"] != 1 {
				t.Fatalf("persisted missing-cwd source with no key admitted stale included owner: %+v", h)
			}

		})
	}
}
