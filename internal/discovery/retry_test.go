package discovery

import (
	"context"
	"errors"
	"strconv"
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
