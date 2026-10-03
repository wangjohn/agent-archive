package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/orbifold"
)

func TestEvalExportNativeFolderOwnership(t *testing.T) {
	t.Parallel()
	home, external := t.TempDir(), t.TempDir()
	env := Env{UserHomeDir: func() (string, error) { return home, nil }, LookupEnv: func(key string) (string, bool) {
		switch key {
		case "CLAUDE_CONFIG_DIR":
			return filepath.Join(external, "claude-root"), true
		case "CODEX_HOME":
			return filepath.Join(external, "codex-root"), true
		}
		return "", false
	}, Agents: fourthRegistry(t, &orbifold.Ports{})}
	x := evalExporter{env: env}
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(home, ".claude", "projects", "p", "s.jsonl"), "claude"},
		{filepath.Join(home, ".codex", "sessions", "s.jsonl"), "codex"},
		{filepath.Join(external, "claude-root", "projects", "s.jsonl"), "claude"},
		{filepath.Join(external, "codex-root", "sessions", "s.jsonl"), "codex"},
		{filepath.Join(home, ".cursor", "projects", "p", "agent-transcripts", "s.txt"), "cursor"},
		{filepath.Join(home, ".orbifold", "flight-recorder", "s.orbit"), "orbifold"},
		{filepath.Join(home, ".orbifold", "other", "s.orbit"), ""},
		{filepath.Join(home, ".codex-other", "s.jsonl"), ""},
		{filepath.Join(home, "unrecognized", "s.jsonl"), ""},
	} {
		if got := x.transcriptHarness(tc.path); got != tc.want {
			t.Errorf("owner(%q)=%q, want %q", tc.path, got, tc.want)
		}
	}
	for _, root := range []string{home, external} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("inference wrote under %s: %v", root, entries)
		}
	}
}

func TestEvalExportAmbiguousNativeFolderRequiresHarness(t *testing.T) {
	t.Parallel()
	home, root := t.TempDir(), t.TempDir()
	x := evalExporter{env: Env{UserHomeDir: func() (string, error) { return home, nil }, LookupEnv: func(key string) (string, bool) {
		if key == "CLAUDE_CONFIG_DIR" || key == "CODEX_HOME" {
			return root, true
		}
		return "", false
	}}}
	if got := x.transcriptHarness(filepath.Join(root, "s.jsonl")); got != "" {
		t.Fatalf("ambiguous owner=%q", got)
	}
}
