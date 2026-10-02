package discoveryio

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io/fs"
	"path/filepath"
)

// Files streams layout-qualified references or compatibility-inspected candidates.
// Native filename duplicate preference is declared by the caller; archive conflicts
// and admission are never decided here.
func Files(ctx context.Context, r agentapi.DiscoveryRequest, id agentmeta.ID, roots []agentapi.NativeStoreRoot, inspector agentapi.NativeHeaderInspector, deduplicateNames bool, emit func(agentapi.DiscoveryCandidate) error) (out agentapi.DiscoveryReport, err error) {
	if r.Files == nil || emit == nil || r.Stage != agentapi.DiscoveryReferences && r.Stage != agentapi.DiscoveryIdentities {
		return out, fmt.Errorf("invalid discovery request")
	}
	if r.Stage == agentapi.DiscoveryIdentities && (r.HeaderBytes <= 0 || r.RecordBytes <= 0) {
		return out, fmt.Errorf("positive discovery header bounds required")
	}
	seen := map[string]bool{}
	unreadActive := false
	for _, root := range roots {
		if e := ctx.Err(); e != nil {
			return out, e
		}
		remaining := 0
		if r.MaxFiles > 0 {
			remaining = r.MaxFiles - out.Enumerated
			if remaining <= 0 {
				out.Incomplete = true
				break
			}
		}
		coverage, e := Walk(ctx, r.Files, root, remaining, func(ref Ref) (bool, error) {
			if deduplicateNames && r.Purpose != agentapi.DiscoveryHandoff && seen[filepath.Base(ref.Path)] {
				return true, nil
			}
			c := agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: id}, Source: agentapi.SourceRef{Path: ref.Path}, Root: root.Path, SourcePriority: root.Priority}
			if r.Stage == agentapi.DiscoveryIdentities {
				info, e := r.Files.Lstat(ref.Path)
				if e != nil || !info.Mode().IsRegular() {
					return true, nil
				}
				c.Bytes = info.Size()
				seen[filepath.Base(ref.Path)] = true
				c.Header, c.IdentityError = inspector.InspectHeader(agentapi.NativeHeaderRequest{Purpose: r.Purpose, Path: ref.Path, Scan: func(visit func([]byte) bool) error {
					return ScanRecords(ctx, r.Files, ref.Path, r.HeaderBytes, r.RecordBytes, visit)
				}})
				c.IdentityInspected = true
				c.Session.NativeID = c.Header.NativeID
				if errors.Is(c.IdentityError, fs.ErrNotExist) {
					return true, nil
				}
			}
			out.Enumerated++
			if e := emit(c); e != nil {
				return false, e
			}
			return true, nil
		})
		out.UnreadableFolders += coverage.UnreadableFolders
		if coverage.RootUnreadable {
			out.StoreUnreadable = true
			if !root.Historical {
				unreadActive = true
			}
		}
		if !coverage.Complete {
			out.Incomplete = true
		}
		if e != nil {
			return out, e
		}
	}
	out.HistoricalOnly = out.StoreUnreadable && !unreadActive
	return out, ctx.Err()
}
