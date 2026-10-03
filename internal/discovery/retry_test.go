package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestConflictingSourceDoesNotStarveLaterDirectoryBatches(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	for n := 1; n <= 600; n++ {
		writeRollout(t, root, project, at.Add(-time.Hour), n, "sessions")
	}
	first, cookie, _, err := readBatch(directory{Root: root, Path: "sessions"})
	if err != nil || len(first) == 0 {
		t.Fatal("missing first fixture batch", err)
	}
	second, _, _, err := readBatch(directory{Root: root, Path: "sessions", Offset: cookie})
	if err != nil || len(second) == 0 {
		t.Fatal("missing later fixture batch", err)
	}
	makeFresh := func(name string) string {
		t.Helper()
		id := sourcefacts.RolloutID(name)
		n, err := strconv.Atoi(id[len(id)-12:])
		if err != nil {
			t.Fatal(err)
		}
		return writeRollout(t, root, project, at.Add(time.Minute), n, "sessions")
	}
	conflict := makeFresh(first[0])
	fresh := makeFresh(second[0])
	otherProject := t.TempDir()
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := store.RegisterOrMerge(sessionKey("codex", conflict), func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: conflict, Harness: archive.Harness{Name: "codex"}, ProjectRoot: otherProject, ProjectID: archive.ProjectID(otherProject), SessionStartedAt: at.Add(time.Minute), AdmittedAt: at.Add(time.Minute), RegisteredAt: at.Add(time.Minute), Origin: archive.SessionOriginHook, DestinationID: cfg.DestinationID()}
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	for range 12 {
		h, err := runScheduledSynthetic(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }})
		if err != nil || h.Probes > HeaderProbes {
			t.Fatalf("unbounded or failed scan: %#v %v", h, err)
		}
		_, admitted, err = store.ArchiveSessionID(sessionKey("codex", fresh))
		if err != nil && !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			t.Fatal(err)
		}
		if admitted {
			break
		}
	}
	if !admitted {
		t.Fatal("conflicting source starved eligible source in a later directory batch")
	}
	after, found, err := store.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || after.ProjectRoot != reg.ProjectRoot || after.TranscriptPath != "" {
		t.Fatal("conflicting source changed original registration", err)
	}
}

// Ordinary enumeration errors are retryable observations, not completed
// coverage. Native probing and all successful enumeration stay real.
type unavailableDirectoryAdapter struct {
	codexAdapter
	attempts   int
	persistent bool
}

func (*unavailableDirectoryAdapter) InitialDirectories() []string {
	return []string{"sessions/blocked", "archived_sessions"}
}

func (*unavailableDirectoryAdapter) PriorityDirectories(time.Time) []string { return nil }

func (a *unavailableDirectoryAdapter) Enumerate(ctx context.Context, root, path string, cookie int64) (SourceBatch, error) {
	if path == "sessions/blocked" {
		a.attempts++
		if a.persistent || a.attempts == 1 {
			return SourceBatch{}, os.ErrPermission
		}
	}
	return a.codexAdapter.Enumerate(ctx, root, path, cookie)
}

func TestUnavailableDirectoryRetriesNextPassWithoutStarvingBacklog(t *testing.T) {
	t.Parallel()
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "persistent"}[persistent], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			project := cfg.Archive.Projects[0].Root
			blocked := writeRollout(t, root, project, at.Add(time.Minute), 9000, "sessions/blocked")
			for n := 1; n <= 600; n++ {
				writeRollout(t, root, project, at.Add(-time.Hour), n, "archived_sessions")
			}
			names, _, _, err := readBatch(directory{Root: root, Path: "archived_sessions"})
			if err != nil || len(names) == 0 {
				t.Fatal("missing backlog", err)
			}
			forward := sourcefacts.RolloutID(names[0])
			raw, err := os.ReadFile(filepath.Join(root, "archived_sessions", names[0]))
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), at.Add(-time.Hour).Format(time.RFC3339Nano), at.Add(time.Minute).Format(time.RFC3339Nano)))
			if err := os.WriteFile(filepath.Join(root, "archived_sessions", names[0]), raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Establish authoritative absence first so this proof isolates directory
			// scheduling from the separate required admission census transition.
			unlock, err := local.NamedLock(store.Home(), "hooks.lock")
			if err != nil {
				t.Fatal(err)
			}
			for _, native := range []string{blocked, forward} {
				if err := store.RequestSessionIndexRecovery(sessionKey("codex", native)); err != nil {
					unlock()
					t.Fatal(err)
				}
			}
			unlock()
			if err := store.RecoverSessionIndexIfNeeded(context.Background()); err != nil {
				t.Fatal(err)
			}
			adapter := &unavailableDirectoryAdapter{codexAdapter: codexAdapter{supported: syntheticSupport}, persistent: persistent}
			opts := Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}
			first, err := runWithAdapters(context.Background(), store, cfg, opts, []SourceAdapter{adapter})
			if err != nil || !first.Pending || first.Registered != 1 || first.Probes > HeaderProbes || adapter.attempts != 1 {
				t.Fatalf("failed directory starved forward work: %#v attempts=%d %v", first, adapter.attempts, err)
			}
			second, err := runWithAdapters(context.Background(), store, cfg, opts, []SourceAdapter{adapter})
			if err != nil || second.Probes > HeaderProbes || adapter.attempts < 2 || persistent && adapter.attempts != 2 {
				t.Fatalf("failed directory not retried next pass: %#v attempts=%d %v", second, adapter.attempts, err)
			}
			_, found, err := store.ArchiveSessionID(sessionKey("codex", blocked))
			if err != nil || found == persistent {
				t.Fatalf("recovered directory admission: found=%v persistent=%v %v", found, persistent, err)
			}
			for pass := 0; pass < 6 && second.Pending; pass++ {
				priorAttempts := adapter.attempts
				second, err = runWithAdapters(context.Background(), store, cfg, opts, []SourceAdapter{adapter})
				if err != nil || second.Probes > HeaderProbes || persistent && adapter.attempts-priorAttempts > 1 {
					t.Fatalf("directory error spun or broke budget: %#v attempts=%d %v", second, adapter.attempts-priorAttempts, err)
				}
				if persistent {
					var c catalog
					if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &c); err != nil {
						t.Fatal(err)
					}
					if len(c.Queue) == 1 && c.Queue[0].Path == "sessions/blocked" {
						return
					}
				}
			}
			if persistent || second.Pending {
				t.Fatal("forward backlog did not complete independently of failed directory")
			}
		})
	}
}
