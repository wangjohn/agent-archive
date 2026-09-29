package collector

import (
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

// repoKey is the registration's repository key: the one recorded when it
// registered, else one derived from its project root (a registration from
// before the field, or one made where git was unavailable), else "".
func (o Options) repoKey(reg archive.SessionRegistration) string {
	if reg.RepoKey != "" {
		return reg.RepoKey
	}
	if reg.ProjectRoot == "" {
		return ""
	}
	cache := o.repoKeys
	if cache == nil {
		// Not inside Run: nothing to share the answer with.
		cache = newRepoKeyCache(o.RepoKey)
	}
	return cache.key(reg.ProjectRoot)
}
