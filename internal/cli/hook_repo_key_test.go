package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// The hook command hands capture the environment's repository lookup, so a
// session registered by `_hook` carries its repository key, and one whose
// lookup finds none registers without, silently.
func TestHookCommandRecordsTheRepoKeyOnRegistration(t *testing.T) {
	t.Parallel()
	want := archive.RepoKey("https://example.test/acme/widget.git")
	for name, tc := range map[string]struct{ lookup func(string) string }{
		"a repository":      {func(string) string { return want }},
		"no repository key": {func(string) string { return "" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, now.Add(-time.Hour))
			env := testEnv(t, home, now)
			env.repoKey = tc.lookup
			payload := `{"hook_event_name":"SessionStart","source":"startup","session_id":"native-1","cwd":` + quoteJSON(project) + `}`
			var errOut bytes.Buffer
			if code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() > 0 {
				t.Fatalf("code %d, stderr %q", code, errOut.String())
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("registrations = %#v, err = %v", regs, err)
			}
			if regs[0].RepoKey != tc.lookup("") {
				t.Errorf("RepoKey = %q, want %q", regs[0].RepoKey, tc.lookup(""))
			}
		})
	}
}

func quoteJSON(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }
