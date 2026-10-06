package archive

import (
	"errors"
	"path/filepath"
	"time"
)

// CodexSourceBinding records validated native facts independently of admission.
// Producer versions may change between physical revisions. Native identity,
// creation and ownership facts remain fixed after they become known.
type CodexSourceBinding struct {
	Child             bool      `json:"child"`
	NativeThreadID    string    `json:"native_thread_id"`
	Version           int       `json:"version"`
	NativeCreatedAt   time.Time `json:"native_created_at"`
	SelectedCwd       string    `json:"selected_cwd,omitempty"`
	Cwd               string    `json:"cwd"`
	ProducerSource    string    `json:"producer_source"`
	RootID            string    `json:"root_id,omitempty"`
	ParentID          string    `json:"parent_id,omitempty"`
	OwnStart          *uint64   `json:"own_start_ordinal,omitempty"`
	PhysicalRolloutID string    `json:"physical_rollout_id"`
	Path              string    `json:"path"`
	Home              string    `json:"home,omitempty"`
}

// Validate checks the bounded private binding before using its locator.
func (b *CodexSourceBinding) Validate() error {
	if b == nil {
		return nil
	}
	if b.Version != 1 || !historyID.MatchString(b.NativeThreadID) || b.NativeCreatedAt.IsZero() || !filepath.IsAbs(b.Cwd) || len(b.Cwd) > 4096 || b.SelectedCwd != "" && (!filepath.IsAbs(b.SelectedCwd) || len(b.SelectedCwd) > 4096) || len(b.ProducerSource) > 4096 || !historyID.MatchString(b.PhysicalRolloutID) || !filepath.IsAbs(b.Path) || len(b.Path) > 4096 || b.RootID != "" && !historyID.MatchString(b.RootID) || b.ParentID != "" && !historyID.MatchString(b.ParentID) || b.Home != "" && (!filepath.IsAbs(b.Home) || len(b.Home) > 4096) {
		return errors.New("invalid Codex source binding")
	}
	return nil
}

// PreservesFacts rejects a new locator that contradicts known native ownership.
func (b *CodexSourceBinding) PreservesFacts(previous *CodexSourceBinding) bool {
	if b == nil {
		return previous == nil
	}
	if previous == nil {
		return true
	}
	return b.Child == previous.Child && b.NativeThreadID == previous.NativeThreadID && b.NativeCreatedAt.Equal(previous.NativeCreatedAt) && b.Cwd == previous.Cwd && b.ProducerSource == previous.ProducerSource && (previous.RootID == "" || b.RootID == previous.RootID) && (previous.ParentID == "" || b.ParentID == previous.ParentID) && (previous.OwnStart == nil || b.OwnStart != nil && *b.OwnStart == *previous.OwnStart) && (previous.Home == "" || b.Home == previous.Home)
}
