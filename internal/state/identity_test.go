package state

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAgentNamespacesKeepSameNativeIdentityDistinct(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ids := map[string]string{}
	for _, agent := range []string{"claude", "codex", "cursor"} {
		reg, err := store.RegisterOrMerge("same-native", func(id string) archive.SessionRegistration {
			r := registrationFor(id)
			r.Harness.Name = agent
			r.NativeSessionID = "same-native"
			return r
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, previous := range ids {
			if previous == reg.ArchiveSessionID {
				t.Fatal("agents collided")
			}
		}
		ids[agent] = reg.ArchiveSessionID
	}
	if _, _, err := store.ArchiveSessionID("same-native"); err == nil {
		t.Fatal("ambiguous legacy lookup resolved arbitrarily")
	}
	for agent, id := range ids {
		got, found, err := store.AgentSessionID(agent, "same-native")
		if err != nil || !found || got != id {
			t.Fatal("namespace lost")
		}
	}
}
func TestLegacyIdentityMigratesOnlyAfterRegistrationOwnership(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	legacy, _, err := store.EnsureArchiveSessionID("native-1")
	if err != nil {
		t.Fatal(err)
	}
	reg := registrationFor(legacy)
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	id, found, err := store.AgentSessionID("claude", "native-1")
	if err != nil || !found || id != legacy {
		t.Fatal("legacy identity changed")
	}
	if _, found, err := store.AgentSessionID("codex", "native-1"); err != nil || found {
		t.Fatal("foreign legacy registration borrowed")
	}
	if err := os.WriteFile(nativeSessionIndexPath(store.home, "claude\x00native-1"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureAgentSessionID("claude", "native-1"); err == nil {
		t.Fatal("corrupt namespace allocated replacement")
	}
}
func TestRegisterOrMergeConcurrentArrivalsPreserveOneProvenance(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := local.NamedLockWait(store.home, "hooks.lock", time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			reg, err := store.RegisterOrMerge("native-1", func(id string) archive.SessionRegistration {
				r := registrationFor(id)
				r.Origin = archive.SessionOriginDiscovery
				r.AdmittedAt = r.SessionStartedAt.Add(time.Hour)
				return r
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- reg.ArchiveSessionID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("duplicate archive identity")
		}
		first = id
	}
	existing, _, _ := store.LoadRegistration(first)
	merged, err := store.RegisterOrMerge("native-1", func(id string) archive.SessionRegistration {
		r := registrationFor(id)
		r.Origin = archive.SessionOriginHook
		r.SessionStartedAt = r.SessionStartedAt.Add(3 * time.Hour)
		r.AdmittedAt = r.SessionStartedAt
		return r
	})
	if err != nil || merged.Origin != existing.Origin || !merged.AdmittedAt.Equal(existing.AdmittedAt) || !merged.SessionStartedAt.Equal(existing.SessionStartedAt) {
		t.Fatal("merge changed immutable facts")
	}
}
