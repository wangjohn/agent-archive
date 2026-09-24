package collector

import (
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// publishedFile mirrors the JSON of published/<id>.json, which package state
// owns, for tests that write local state as an earlier release left it. It
// is a copy on purpose: a change to the on-disk format that breaks reading
// what earlier releases wrote shows up here as a failing test.
type publishedFile struct {
	MetadataBytes        []byte                         `json:"metadata_bytes,omitempty"`
	Bundle               archive.SourceBundle           `json:"bundle"`
	PublishedAt          time.Time                      `json:"published_at"`
	Status               state.CacheStatus              `json:"status"`
	BlockedReason        state.BlockedReason            `json:"blocked_reason,omitempty"`
	PreBlockStatus       state.CacheStatus              `json:"pre_block_status,omitempty"`
	DeferredHookEvidence []archive.SupplementalEvidence `json:"deferred_hook_evidence,omitempty"`
	LastPublished        *publishedFileSnapshot         `json:"last_published,omitempty"`
}

type publishedFileSnapshot struct {
	Bundle       archive.SourceBundle     `json:"bundle"`
	PublishedAt  time.Time                `json:"published_at"`
	Source       *archive.SourceReference `json:"source,omitempty"`
	SameAsBundle bool                     `json:"same_as_bundle,omitempty"`
}

// Paths of a store's files, for tests that damage or inspect them.
func publishedPath(s *state.Store, id string) string {
	return filepath.Join(s.Home(), "published", id+".json")
}

func requestPath(s *state.Store, id string) string {
	return filepath.Join(s.Home(), "requests", id+".json")
}

func registrationPath(s *state.Store, id string) string {
	return filepath.Join(s.Home(), "registrations", id+".json")
}

func subagentCandidatePath(s *state.Store, id string) string {
	return filepath.Join(s.Home(), "subagent-candidates", id+".json")
}
