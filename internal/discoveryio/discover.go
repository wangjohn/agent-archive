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
			var c agentapi.DiscoveryCandidate
			if r.Stage == agentapi.DiscoveryIdentities {
				candidate, valid := inspectCandidate(ctx, r, id, root, ref, inspector)
				if !valid {
					return true, nil
				}
				c = candidate
				seen[filepath.Base(ref.Path)] = true
				if !candidatePresent(c) {
					return true, nil
				}
			} else {
				c = agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: id}, Source: agentapi.SourceRef{Path: ref.Path}, Root: root.Path, SourcePriority: root.Priority}
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

// inspectCandidate keeps unavailable files out of enumeration while retaining
// other bounded identity errors as local candidate evidence.
func inspectCandidate(ctx context.Context, r agentapi.DiscoveryRequest, id agentmeta.ID, root agentapi.NativeStoreRoot, ref Ref, inspector agentapi.NativeHeaderInspector) (agentapi.DiscoveryCandidate, bool) {
	info, err := r.Files.Lstat(ref.Path)
	if err != nil || !info.Mode().IsRegular() {
		return agentapi.DiscoveryCandidate{}, false
	}
	header, identityErr := inspector.InspectHeader(agentapi.NativeHeaderRequest{Purpose: r.Purpose, Path: ref.Path, Scan: func(visit func([]byte) bool) error {
		return ScanRecords(ctx, r.Files, ref.Path, r.HeaderBytes, r.RecordBytes, visit)
	}})
	return agentapi.DiscoveryCandidate{Session: agentapi.NativeSession{Agent: id, NativeID: header.NativeID}, Source: agentapi.SourceRef{Path: ref.Path}, Root: root.Path, SourcePriority: root.Priority, Bytes: info.Size(), Header: header, IdentityInspected: true, IdentityError: identityErr}, true
}

func candidatePresent(c agentapi.DiscoveryCandidate) bool {
	return !errors.Is(c.IdentityError, fs.ErrNotExist)
}
