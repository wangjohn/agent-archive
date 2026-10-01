package collector

import (
	"io"
	"os"
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
// subagent's, the file is missing, unreadable, a symbolic link or otherwise
// not a regular file, or larger than archive.MaxSubagentMetaBytes. The file
// is optional, so none of these is an error, a warning, or a gap. What the
// filter keeps of its contents is decided there, not here.
//
// A link is refused, not followed: what is read is the file Claude Code wrote
// beside the transcript, never another file elsewhere that a link names (one
// with a description of its own, such as a package.json). The file opened is
// checked to be the one found, so a link put in its place meanwhile is not
// read either.
func readSubagentMeta(transcriptPath string) []byte {
	path, ok := archive.SubagentMetaPath(transcriptPath)
	if !ok {
		return nil
	}
	subagentMetaReads.Add(1)
	found, err := os.Lstat(path)
	if err != nil || !found.Mode().IsRegular() {
		return nil
	}
	file, err := openRegularFile(path)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	if opened, err := file.Stat(); err != nil || !os.SameFile(found, opened) {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, archive.MaxSubagentMetaBytes+1))
	if err != nil || len(data) > archive.MaxSubagentMetaBytes {
		return nil
	}
	return data
}
