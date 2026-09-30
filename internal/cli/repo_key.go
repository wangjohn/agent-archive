package cli

import "github.com/wangjohn/agent-archive/internal/gitremote"

// repoKeyResolver returns what looks up a project's repository key (a hash of
// its git origin, see archive.RepoKey): git run with a short timeout, or a
// test's stand-in. It never fails; a project without a key gets "". The hook
// runtime and backfill are handed this, since neither may run a program on
// its own.
func (e Env) repoKeyResolver() func(root string) string {
	if e.repoKey != nil {
		return e.repoKey
	}
	return (&gitremote.Resolver{}).Key
}
