package agentapi

import (
	"context"
	"time"
)

// DatabaseChat is compact native catalog evidence for a possible historical session.
// KeyID is the native record locator; identity disagreement is not silently repaired.
type DatabaseChat struct {
	ID, KeyID           string
	CreatedAt           time.Time
	Folder, WorkspaceID string
	Malformed           bool
}

// DatabaseCatalog retains only compact planning metadata, never raw message content.
// A native catalog may need parent IDs before excluding child sessions.
type DatabaseCatalog struct {
	Chats       []DatabaseChat
	NewerFormat int
	Subagents   map[string][]string
}

// DatabaseRows supplies a read-only query cursor whose lifetime ends at Close.
type DatabaseRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

// DatabaseCatalogHost allows a native owner to choose its indexed metadata query.
// The shared host owns database opening, locks, snapshots and cleanup.
type DatabaseCatalogHost interface {
	Query(context.Context, string) (DatabaseRows, error)
}

// DatabaseCatalogInspector owns native catalog schema and metadata interpretation.
type DatabaseCatalogInspector interface {
	InspectCatalog(context.Context, DatabaseCatalogHost) (DatabaseCatalog, error)
}

// DatabaseCatalogLookup projects implemented native metadata inventory owners.
type DatabaseCatalogLookup interface {
	LookupDatabaseCatalog(string) (DatabaseCatalogInspector, bool)
}
