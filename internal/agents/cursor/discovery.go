package cursor

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

// Discovery owns Cursor's native file layout and JSONL-over-text preference.
// Automatic interactive native discovery remains unavailable for Cursor.
type Discovery struct{}

// Discover emits import candidates directly, retaining workspace slug evidence.
func (Discovery) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (out agentapi.DiscoveryReport, err error) {
	if r.Purpose == agentapi.DiscoveryProjects {
		return out, nil
	}
	if r.Purpose != agentapi.DiscoveryImport {
		return out, fmt.Errorf("native discovery purpose unavailable")
	}
	if r.Files == nil || emit == nil {
		return out, fmt.Errorf("discovery dependencies required")
	}
	list := func(path string, root bool) ([]fs.DirEntry, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		entries, e := r.Files.ReadDir(path)
		if e != nil {
			if !errors.Is(e, fs.ErrNotExist) && !errors.Is(e, syscall.ENOTDIR) {
				out.Incomplete = true
				if root {
					out.StoreUnreadable = true
				} else {
					out.UnreadableFolders++
				}
			}
			return nil, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		return entries, nil
	}
	root := filepath.Join(r.Locations.UserHome, ".cursor", "projects")
	slugs, e := list(root, true)
	if e != nil {
		return out, e
	}
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		dir := filepath.Join(root, slug.Name(), "agent-transcripts")
		entries, e := list(dir, false)
		if e != nil {
			return out, e
		}
		// Only one workspace's compact path inventory is needed to prefer JSONL.
		chats := map[string]agentapi.DiscoveryCandidate{}
		var order []string
		for _, entry := range entries {
			if e := ctx.Err(); e != nil {
				return out, e
			}
			var id, path string
			switch {
			case entry.IsDir():
				id = entry.Name()
				path = filepath.Join(dir, id, id+".jsonl")
			case entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".txt"):
				id = strings.TrimSuffix(entry.Name(), ".txt")
				path = filepath.Join(dir, entry.Name())
			default:
				continue
			}
			if id == "" {
				continue
			}
			info, e := r.Files.Lstat(path)
			if e != nil || !info.Mode().IsRegular() {
				continue
			}
			if prior, ok := chats[id]; ok {
				if strings.HasSuffix(prior.Source.Path, ".jsonl") {
					continue
				}
			} else {
				order = append(order, id)
			}
			chats[id] = agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: agentmeta.Cursor, NativeID: id}, Source: agentapi.SourceRef{Path: path}, Root: root, Bytes: info.Size(), IdentityInspected: true, WorkspaceKey: slug.Name()}
		}
		for _, id := range order {
			if e := ctx.Err(); e != nil {
				out.Incomplete = true
				return out, e
			}
			if r.MaxFiles > 0 && out.Enumerated >= r.MaxFiles {
				out.Incomplete = true
				return out, nil
			}
			out.Enumerated++
			if e := emit(chats[id]); e != nil {
				return out, e
			}
		}
	}
	return out, ctx.Err()
}

// DefaultDirectories declares native configuration without changing purpose-specific coverage.
func (Discovery) DefaultDirectories(home string) []string {
	return []string{filepath.Join(home, ".cursor")}
}
