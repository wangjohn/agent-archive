package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

// The hook command hands capture the environment's commit lookup, so `_hook`
// records the commit a session started on, and the last one a stop saw.
func TestHookCommandRecordsTheStartingAndLastCommit(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	env := testEnv(t, home, now)
	env.repoKey = func(string) string { return "" }
	sha := strings.Repeat("3f", 20)
	var asked []bool
	env.gitHead = func(dir string, withDirty bool) (string, *bool) {
		if dir != project {
			t.Errorf("git was asked about %s, want the session's directory %s", dir, project)
		}
		asked = append(asked, withDirty)
		return sha, new(true)
	}
	for _, payload := range []string{
		`{"hook_event_name":"SessionStart","source":"startup","session_id":"native-1","cwd":` + quoteJSON(project) + `}`,
		`{"hook_event_name":"Stop","session_id":"native-1","cwd":` + quoteJSON(project) + `}`,
	} {
		var errOut bytes.Buffer
		if code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() > 0 {
			t.Fatalf("code %d, stderr %q", code, errOut.String())
		}
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v, err = %v", regs, err)
	}
	reg := regs[0]
	if reg.StartHead == nil || reg.StartHead.SHA != sha || reg.StartHead.Dirty == nil || !*reg.StartHead.Dirty {
		t.Errorf("StartHead = %+v", reg.StartHead)
	}
	if reg.LastHead == nil || reg.LastHead.SHA != sha || reg.LastHead.Dirty != nil {
		t.Errorf("LastHead = %+v", reg.LastHead)
	}
	if len(asked) != 2 || !asked[0] || asked[1] {
		t.Errorf("git was asked with dirty %v, want [true false]", asked)
	}
}
