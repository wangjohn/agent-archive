package archive

import (
	"regexp"
	"time"
)

// gitObjectName is a full git object name: SHA-1 (40 hex digits) or SHA-256
// (64). An abbreviation is not enough to check a commit out unambiguously.
var gitObjectName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// IsGitObjectName reports whether s is a full, lowercase git object name.
func IsGitObjectName(s string) bool { return gitObjectName.MatchString(s) }

// GitHead is what a hook saw checked out in the session's working directory:
// the commit HEAD resolved to, and, where it was asked, whether the working
// tree differed from it. It holds no path, branch, remote, or file name.
type GitHead struct {
	// SHA is HEAD's full object name (see IsGitObjectName).
	SHA string `json:"sha"`
	// Dirty is true when the working tree had staged, unstaged, or untracked
	// (not ignored) changes against SHA, false when it had none, and nil when
	// it was not asked or git could not tell in time.
	Dirty *bool `json:"dirty,omitempty"`
	// ObservedAt is the time of the hook event that saw it.
	ObservedAt time.Time `json:"observed_at"`
}

// Valid reports whether h names a commit and a time.
func (h *GitHead) Valid() bool {
	return h != nil && IsGitObjectName(h.SHA) && !h.ObservedAt.IsZero()
}

// SessionGitHead is the metadata's git_head: the commit the session's
// working directory had checked out when it started, and the last one a
// stop hook saw. Either is absent when no hook could tell; neither is ever
// derived after the fact.
type SessionGitHead struct {
	// Start is HEAD, with Dirty, when the hook that registered the session
	// ran.
	Start *GitHead `json:"start,omitempty"`
	// Last is HEAD at the most recent stop hook that could read it, from the
	// first stop that saw HEAD at that commit (ObservedAt). It carries no
	// Dirty.
	Last *GitHead `json:"last,omitempty"`
}

// ApplyGitHead copies the git state the session's hooks recorded on its
// registration into the metadata, in UTC. Anything that is not a full object
// name with a time is left out, so nothing else can reach the sidecar through
// this field, and a registration without either leaves git_head absent.
func (m *Metadata) ApplyGitHead(r SessionRegistration) {
	head := SessionGitHead{Start: publishedGitHead(r.StartHead, true), Last: publishedGitHead(r.LastHead, false)}
	if head.Start == nil && head.Last == nil {
		m.GitHead = nil
		return
	}
	m.GitHead = &head
}

func publishedGitHead(h *GitHead, withDirty bool) *GitHead {
	if !h.Valid() {
		return nil
	}
	var dirty *bool
	if withDirty && h.Dirty != nil {
		dirty = new(*h.Dirty)
	}
	return &GitHead{SHA: h.SHA, Dirty: dirty, ObservedAt: h.ObservedAt.UTC()}
}
