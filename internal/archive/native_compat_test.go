package archive

import (
	"fmt"
	"path/filepath"
	"strings"
)

// NewAdapter returns a privacy-first adapter by canonical harness name.
func NewAdapter(name string) (Adapter, error) {
	switch CanonicalHarness(name) {
	case HarnessCodex:
		return CodexAdapter{}, nil
	case HarnessClaude:
		return ClaudeAdapter{}, nil
	case HarnessCursor:
		return CursorAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported archive adapter %q", name)
	}
}

// SubagentMetaPath is the .meta.json beside a Claude Code subagent
// transcript: agent-<id>.jsonl names agent-<id>.meta.json in the same
// directory. ok is false for any other file name, which is not a subagent
// transcript.
func SubagentMetaPath(transcriptPath string) (path string, ok bool) {
	dir, name := filepath.Split(transcriptPath)
	id, found := strings.CutPrefix(name, "agent-")
	if !found {
		return "", false
	}
	id, found = strings.CutSuffix(id, ".jsonl")
	if !found || id == "" {
		return "", false
	}
	return filepath.Join(dir, "agent-"+id+".meta.json"), true
}
