package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// collectNativeFixture supplies the production lookup, home confinement and
// bounded observation stages before judging a relocated native registration.
func collectNativeFixture(t *testing.T, store *state.Store, bucket storage.ObjectStore, home string, now time.Time) collector.Result {
	t.Helper()
	cfg, found, err := config.Load(store.Home())
	if err != nil || !found {
		t.Fatal("fixture configuration", err)
	}
	var result collector.Result
	for range 8 {
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{home})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Run(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Rollouts: lookup, Now: func() time.Time { return now }})
		if err == nil {
			result, err = collector.Run(context.Background(), store, bucket, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), CodexRollouts: lookup, ConfiguredCodexHomes: []string{home}, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }})
		}
		if err = errors.Join(err, lookup.Close()); err != nil {
			t.Fatal(err)
		}
		if len(result.Published) > 0 {
			return result
		}
	}
	for _, issue := range result.Errors {
		var source *agentapi.SourceError
		if errors.As(issue, &source) {
			t.Log("native provider pending", source.Kind, source.Err)
		}
	}
	return result
}
