package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Every command taking --harness rejects an unknown app with one tidy line
// and exit 2, before touching storage.
func TestHarnessFlagRejectsUnknownApps(t *testing.T) {
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{
		{"list", "--harness", "nope"},
		{"list", "--json", "--harness", "nope"},
		{"show", "some-id", "--harness", "nope"},
		{"handoff", "some-id", "--harness", "nope"},
		{"backfill", "--dry-run", "--harness", "nope"},
	} {
		var out, errOut bytes.Buffer
		code := Run(args, nil, &out, &errOut, env)
		want := "agent-archive: " + args[0] + `: --harness must be claude, codex, or cursor, not "nope"; run agent-archive ` + args[0] + " --help\n"
		if code != 2 || errOut.String() != want || out.Len() != 0 {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want exit 2 and %q", args, code, out.String(), errOut.String(), want)
		}
	}
}

// "claude-code" names Claude, as hooks and backfill already accept, and
// filters as "claude" does rather than matching nothing.
func TestHarnessFlagAcceptsCanonicalAliases(t *testing.T) {
	for value, want := range map[string]string{"": "", "claude": "claude", "claude-code": "claude", " Codex ": "codex", "cursor": "cursor"} {
		if got, ok := harnessFlag(value); !ok || got != want {
			t.Errorf("harnessFlag(%q) = %q, %v; want %q", value, got, ok, want)
		}
	}
	env, _, id := publishedFixture(t)
	var byName, byAlias, errOut bytes.Buffer
	if code := Run([]string{"list", "--harness", "codex"}, nil, &byName, &errOut, env); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(byName.String(), id) {
		t.Fatalf("list --harness codex:\n%s", byName.String())
	}
	if code := Run([]string{"list", "--harness", "CODEX"}, nil, &byAlias, &errOut, env); code != 0 || byAlias.String() != byName.String() {
		t.Fatalf("list --harness CODEX differs: exit=%d\n%s", code, byAlias.String())
	}
}

// Unset times are left out of status --json rather than printed as the
// zero time (0001-01-01T00:00:00Z).
func TestStatusJSONOmitsUnsetTimes(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StorageVerifiedAt = time.Time{}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("exit=%d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "0001-01-01") {
		t.Fatalf("status --json prints a zero time:\n%s", out.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["storage_verified_at"]; present {
		t.Fatalf("unset storage_verified_at is present: %v", doc["storage_verified_at"])
	}
	if _, present := doc["storage_access_confirmed_at"]; !present {
		t.Fatalf("a set time was dropped")
	}
	// The collector's own status file: a pass that never published has no
	// last_published_at.
	encoded, err := json.Marshal(state.Status{LastScanAt: now})
	if err != nil || strings.Contains(string(encoded), "last_published_at") {
		t.Fatalf("state.Status encodes an unset last_published_at: %s %v", encoded, err)
	}
}
