package collector

import (
	"encoding/json"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/gitremote"
)

// repoKeyCache remembers, for one pass, each project root's repository key,
// so a refresh sweep over many sessions of one project asks git once, not
// once per session.
type repoKeyCache struct {
	lookup func(root string) string
	mu     sync.Mutex
	keys   map[string]string
}

// newRepoKeyCache caches lookup, or the real git lookup when it is nil.
func newRepoKeyCache(lookup func(root string) string) *repoKeyCache {
	if lookup == nil {
		lookup = (&gitremote.Resolver{}).Key
	}
	return &repoKeyCache{lookup: lookup, keys: map[string]string{}}
}

func (c *repoKeyCache) key(root string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[root]; ok {
		return key
	}
	key := c.lookup(root)
	c.keys[root] = key
	return key
}

// repoKeyOr is the registration's repository key: the one recorded when it
// registered, else one derived from its project root (a registration from
// before the field, or one made where git was unavailable), else what prior
// returns, the key already published for the session. The last step makes a
// derived key sticky: a lookup that fails this time (git briefly unavailable,
// the checkout gone) does not drop the key from the republished sidecar,
// while a lookup that succeeds with a different answer (the remote really
// changed) replaces it. A key recorded on the registration is never
// re-derived.
func (o Options) repoKeyOr(reg archive.SessionRegistration, prior func() string) string {
	if reg.RepoKey != "" {
		return reg.RepoKey
	}
	if reg.ProjectRoot != "" && reg.AdmissionStage == "" {
		cache := o.repoKeys
		if cache == nil {
			// Not inside Run: nothing to share the answer with.
			cache = newRepoKeyCache(o.RepoKey)
		}
		if key := cache.key(reg.ProjectRoot); key != "" {
			return key
		}
	}
	if prior == nil {
		return ""
	}
	return prior()
}

// priorRepoKey is the repository key of the metadata last published for the
// session, "" when there is none. It reads the cached copy, which every
// publication refreshes.
func (s *sessionScan) priorRepoKey() string {
	var prior struct {
		RepoKey string `json:"repo_key"`
	}
	if encoded := s.published.Metadata(); len(encoded) > 0 && json.Unmarshal(encoded, &prior) == nil {
		return prior.RepoKey
	}
	return ""
}
