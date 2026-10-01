package discovery

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/local"
	"path/filepath"
	"time"
)

// Date directories are scheduling hints only. Every candidate still passes
// native metadata, origin-support, ownership and durable authorization checks.
// Reserve at least 192 header probes and half the entry budget for fair backlog.
func (s scan) observeActiveHints(ctx context.Context, o Options, roots []string, deadline time.Time) {
	if s.catalog.Priority == nil {
		s.catalog.Priority = map[string]int64{}
	}
	hints := map[string]directory{}
	for _, root := range roots {
		for _, delta := range []int{0, -1, 1} {
			d := directory{Root: root, Path: filepath.Join("sessions", s.now.AddDate(0, 0, delta).Format("2006/01/02"))}
			key := filepath.Join(root, d.Path)
			hints[key] = d
		}
	}
	// Expire priority pinning when roots/dates move; no retention based on names.
	for key := range s.catalog.Priority {
		if _, ok := hints[key]; !ok {
			delete(s.catalog.Priority, key)
		}
	}
	for key, entry := range s.catalog.Cache {
		if entry.ActiveHint {
			pinned := false
			for hint := range hints {
				if local.PathWithin(key, hint) {
					pinned = true
					break
				}
			}
			if !pinned {
				entry.ActiveHint = false
				s.catalog.Cache[key] = entry
			}
		}
	}
	for _, root := range roots {
		for _, delta := range []int{0, -1, 1} {
			d := directory{Root: root, Path: filepath.Join("sessions", s.now.AddDate(0, 0, delta).Format("2006/01/02"))}
			key := filepath.Join(root, d.Path)
			d.Offset = s.catalog.Priority[key]
			for s.health.Probes < 64 && s.health.Entries < 1024 && time.Now().Before(deadline) && !scanStopped(ctx, o) {
				names, next, finished, err := readBatch(d)
				if err != nil {
					s.health.Errors = appendUnique(s.health.Errors, "priority_source_unavailable")
					break
				}
				if finished {
					s.catalog.Priority[key] = 0
					break
				}
				retry := false
				for _, name := range names {
					if s.health.Probes >= 64 || s.health.Entries >= 1024 || scanStopped(ctx, o) || time.Now().After(deadline) {
						retry = true
						break
					}
					again, stop := s.visit(d, name)
					retry = retry || again
					if stop {
						break
					}
				}
				if retry {
					break
				}
				d.Offset = next
				s.catalog.Priority[key] = next
			}
		}
	}
}
