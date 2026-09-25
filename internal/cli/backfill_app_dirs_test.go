package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Regression: 2026-09 review B-26. Setup installs Claude Code's and Codex's
// hooks where CLAUDE_CONFIG_DIR and CODEX_HOME point, but backfill only ever
// looked in ~/.claude and ~/.codex, so a person with those variables set
// imported nothing and was told nothing. Backfill now looks where the
// variables point in its own environment, and where setup recorded
// installing the hooks (for a shell without them), besides the defaults.
func TestBackfillHonorsClaudeConfigDirAndCodexHome(t *testing.T) {
	f := newBackfillFixture(t)
	repo := filepath.Join(f.userHome, "agent-archive")
	claudeAlt := filepath.Join(f.root, "claude-alt")
	codexRecorded := filepath.Join(f.root, "codex-recorded")
	claudeLine := func(id string) string {
		return fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":"2026-09-21T09:00:00Z","message":{"role":"user","content":"please check it"}}`+"\n", id, repo)
	}
	const codexID = "0a9b3c4d-0000-4000-8000-0000000000bb"
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(claudeAlt, "projects", "slug", "c-alt.jsonl"), claudeLine("c-alt"))
	// The same session in both folders is imported once.
	write(filepath.Join(claudeAlt, "projects", "slug", "c-aa-1.jsonl"), claudeLine("c-aa-1"))
	write(filepath.Join(codexRecorded, "sessions", "2026", "09", "21", "rollout-2026-09-21T10-00-00-"+codexID+".jsonl"),
		fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-09-21T17:00:00Z","payload":{"id":%q,"timestamp":"2026-09-21T17:00:00Z","cwd":%q,"source":"cli"}}
{"type":"response_item","timestamp":"2026-09-21T17:00:05Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect it"}]}}
`, codexID, repo))
	f.nativeIDs = append(f.nativeIDs, "c-alt", codexID)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HookFiles = map[string]string{"codex": filepath.Join(codexRecorded, "hooks.json")}
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}

	before := planJSONFor(t, f)
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == "CLAUDE_CONFIG_DIR" {
			return claudeAlt, true
		}
		return "", false
	}
	out, errOut, code := f.command(t, "backfill", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errOut)
	}
	f.checkPrivate(t, "plan JSON", out)
	after := planJSONFor(t, f)
	sessions := func(plan map[string]any, app string) int {
		n := 0
		for _, p := range plan["projects"].([]any) {
			n += int(p.(map[string]any)["sessions"].(map[string]any)[app].(float64))
		}
		return n
	}
	if got, was := sessions(after, "claude"), sessions(before, "claude"); got != was+1 {
		t.Fatalf("claude sessions %d, before CLAUDE_CONFIG_DIR %d; want one more", got, was)
	}
	// The recorded Codex folder is found with or without the variable.
	if got := sessions(before, "codex"); got != 2 {
		t.Fatalf("codex sessions %d, want the default one and the recorded one", got)
	}
	if dup := after["skipped"].(map[string]any)["duplicate_session"]; dup == nil || dup.(float64) != 1 {
		t.Fatalf("skipped %v, want the session in both Claude folders counted once as a duplicate", after["skipped"])
	}
	if strings.Contains(out, claudeAlt) || strings.Contains(out, codexRecorded) {
		t.Fatalf("the plan names an app folder:\n%s", out)
	}
}

// Regression: 2026-09 review B-26. storage_checked is always false in the
// dry-run JSON, which docs/reference/json-output.md now says; no transcript
// path or session ID is in it.
func TestBackfillDryRunJSONStorageCheckedAndPrivacy(t *testing.T) {
	f := newBackfillFixture(t)
	out, _, code := f.command(t, "backfill", "--dry-run", "--json")
	if code != 0 || !strings.Contains(out, `"storage_checked": false`) {
		t.Fatalf("code %d:\n%s", code, out)
	}
	f.checkPrivate(t, "plan JSON", out)
}
