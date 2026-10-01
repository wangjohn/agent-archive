package collector

import (
	"io"
	"sync/atomic"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// subagentMetaReads counts the .meta.json files filterReader tried to read,
// which only a Claude Code subagent's transcript does; tests use the count to
// show no other transcript reads one.
var subagentMetaReads atomic.Int64

// filterReader filters the transcript bytes r of reg with adapter. A Claude
// Code subagent, a registration with a parent whose transcript is named
// agent-<id>.jsonl, also gives the filter the contents of its sibling
// agent-<id>.meta.json, whose description is what the parent called the task
// (see archive.ClaudeAdapter.FilterSubagentJSONL). Every other transcript is
// filtered as it always was, and no other file is read.
func filterReader(adapter archive.Adapter, reg archive.SessionRegistration, r io.Reader) (archive.FilteredTranscript, error) {
	if claude, ok := adapter.(archive.ClaudeAdapter); ok && reg.ParentSessionID != "" {
		if meta := readSubagentMeta(reg.TranscriptPath); meta != nil {
			return claude.FilterSubagentJSONL(r, meta)
		}
	}
	return adapter.FilterJSONL(r)
}

// readSubagentMeta reads the .meta.json beside a subagent transcript, or
// returns nil when there is none to use: the transcript is not named like a
// subagent's, the file is missing, unreadable, not a regular file, or larger
// than archive.MaxSubagentMetaBytes. The file is optional, so none of these
// is an error, a warning, or a gap. What the filter keeps of its contents is
// decided there, not here.
func readSubagentMeta(transcriptPath string) []byte {
	path, ok := archive.SubagentMetaPath(transcriptPath)
	if !ok {
		return nil
	}
	subagentMetaReads.Add(1)
	file, err := openRegularFile(path)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, archive.MaxSubagentMetaBytes+1))
	if err != nil || len(data) > archive.MaxSubagentMetaBytes {
		return nil
	}
	return data
}
