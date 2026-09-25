package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
)

// storageClockFollows makes the retention sweep's storage clock read now()
// and drops its check against the previous pass, for a test that moves time
// forward by changing Env.Now. Without it the sweep sees only this Mac's
// clock jump ahead of a MemoryStore's real one and, rightly, deletes nothing.
func storageClockFollows(t *testing.T, env *Env, now func() time.Time) {
	t.Helper()
	env.sweepClock = func(opts *retention.Options) {
		opts.ServerClock = func(context.Context) (time.Time, error) { return now(), nil }
		opts.PreviousScanAt = time.Time{}
	}
}

// A pass whose clock has jumped months ahead of the storage service's
// deletes nothing and says so in status; the session and its objects stay.
func TestPassWithAClockAheadOfStorageDeletesNothingAndSaysWhy(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, bucket := publishedThroughSync(t, now)
	before, err := bucket.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(91 * 24 * time.Hour) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	if _, still := onlyRegistration(t, home); !still {
		t.Fatal("a clock three months ahead expired the session")
	}
	after, err := bucket.List(context.Background(), "")
	if err != nil || len(after) != len(before) {
		t.Fatalf("objects before=%d after=%d (%v)", len(before), len(after), err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil || !strings.Contains(status.LastError, "clock is ahead") {
		t.Fatalf("status.LastError = %q (%v)", status.LastError, err)
	}
}
