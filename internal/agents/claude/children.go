package claude

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Children owns native subagent transcript association with a discovered parent.
type Children struct{}

func (Children) DiscoverChildren(ctx context.Context, r agentapi.ChildDiscoveryRequest, emit func(agentapi.ChildCandidate) error) (out agentapi.DiscoveryReport, err error) {
	if r.Parent.Agent != agentmeta.Claude || r.Files == nil || emit == nil {
		return out, fmt.Errorf("invalid child discovery request")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	dir := filepath.Join(filepath.Dir(r.Source.Path), r.Parent.NativeID, "subagents")
	entries, err := r.Files.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			out.UnreadableFolders++
			out.Incomplete = true
		}
		return out, ctx.Err()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := r.Files.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		out.Enumerated++
		if err := emit(agentapi.ChildCandidate{NativeID: id, Source: agentapi.SourceRef{Path: path}, Bytes: info.Size()}); err != nil {
			out.Incomplete = true
			return out, err
		}
	}
	return out, ctx.Err()
}
