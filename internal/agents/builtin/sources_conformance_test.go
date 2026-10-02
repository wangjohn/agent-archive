package builtin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestBuiltinFileSourceConformance(t *testing.T) {
	t.Parallel()
	registry := NewBuiltins()
	for _, name := range []string{"claude", "codex", "cursor"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "fixture.jsonl")
			raw := []byte("{\"a\":1}\n{\"b\":2}\n")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			provider, _, _ := registry.LookupSources(name)
			agenttest.FileSource(t, provider, agentapi.SourceRef{Path: path}, raw)
		})
	}
}
