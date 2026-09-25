package reader

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// LinkedAvailability is a read-time observation, separate from the historical
// link in a source bundle. Metadata availability does not verify child source
// bytes; show CHILD --normalized performs that check on explicit selection.
type LinkedAvailability struct {
	SessionID string      `json:"session_id"`
	State     LinkedState `json:"state"`
}

// LinkedState is what a read found for one linked session.
type LinkedState string

const (
	// LinkedStateUnavailable means the link is not a subagent link, or the
	// parent recorded the child as unavailable.
	LinkedStateUnavailable LinkedState = "unavailable"
	// LinkedStateIdentityMismatch means the child's key or metadata does not
	// describe a child of this parent.
	LinkedStateIdentityMismatch LinkedState = "identity_mismatch"
	// LinkedStatePending means the child has not published yet.
	LinkedStatePending LinkedState = "pending"
	// LinkedStateUnavailableOrExpired means the child's metadata is gone.
	LinkedStateUnavailableOrExpired LinkedState = "unavailable_or_expired"
	// LinkedStateLookupFailed means reading the child's metadata failed.
	LinkedStateLookupFailed LinkedState = "lookup_failed"
	// LinkedStateMetadataAvailable means the child's metadata is readable and
	// matches the parent.
	LinkedStateMetadataAvailable LinkedState = "metadata_available"
)

// ResolveLinkedSessions reads only direct child metadata. It does not follow
// grandchildren, download transcripts, or pin children against retention.
// One missing or malformed child must not make its parent's metadata unreadable.
func ResolveLinkedSessions(ctx context.Context, store storage.ObjectStore, parent archive.Metadata) []LinkedAvailability {
	var result []LinkedAvailability
	for _, link := range parent.LinkedSessions {
		item := LinkedAvailability{SessionID: link.SessionID, State: LinkedStateUnavailable}
		if link.Relationship != "subagent" || link.Status == archive.LinkedSessionUnavailable {
			result = append(result, item)
			continue
		}
		key, err := archive.MetadataObjectKey(parent.Harness.Name, link.SessionID)
		if err != nil || link.SessionID == parent.SessionID {
			item.State = LinkedStateIdentityMismatch
			result = append(result, item)
			continue
		}
		child, err := ReadMetadata(ctx, store, key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			if link.Status == archive.LinkedSessionPending {
				item.State = LinkedStatePending
			} else {
				item.State = LinkedStateUnavailableOrExpired
			}
		case err != nil:
			item.State = LinkedStateLookupFailed
		case child.SessionID != link.SessionID || child.ParentSessionID != parent.SessionID || child.ProjectID != parent.ProjectID || child.MachineID != parent.MachineID || child.Harness.Name != parent.Harness.Name:
			item.State = LinkedStateIdentityMismatch
		default:
			item.State = LinkedStateMetadataAvailable
		}
		result = append(result, item)
	}
	return result
}
