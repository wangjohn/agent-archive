package backfill

import "time"

// KnownProject is a project that sessions on this machine ran in, resolved as a
// plan resolves them.
type KnownProject struct {
	Root string
	Kind ProjectKind
	// LastUsed is when a session in it last changed: the newest transcript's
	// modification time.
	LastUsed time.Time
	Sessions int
}
