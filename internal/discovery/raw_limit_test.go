package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// A Codex rollout past the former 128 MiB proof limit, but within the Codex
// raw limit capture uses, is admitted: discovery refuses nothing capture
// reads. Most of it is command output, as in a multi-day thread.
func TestDiscoveryAdmitsCodexRolloutOverFormerProofLimit(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(file, 1<<20)
	line, err := json.Marshal(map[string]any{"type": "event_msg", "timestamp": at.Add(time.Minute).Format(time.RFC3339Nano), "payload": map[string]any{"type": "exec_command_end", "call_id": "c", "aggregated_output": strings.Repeat("output line\n", 1<<16)}})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	for written := int64(0); written <= 130<<20; written += int64(len(line)) {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= 128<<20 || info.Size() > archive.MaxCodexRawTranscriptBytes {
		t.Fatalf("fixture size: %v %v", info, err)
	}
	h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatalf("health=%#v err=%v", h, err)
	}
}
