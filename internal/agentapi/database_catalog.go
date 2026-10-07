package agentapi

import (
	"context"
	"time"
)

// DatabaseChat is compact native catalog evidence for a possible historical session.
// KeyID is the native record locator; identity disagreement is not silently repaired.
type DatabaseChat struct {
	CursorFacts CursorComposerFacts
	ID          string
	KeyID       string
	CreatedAt   time.Time
	Folder      string
	WorkspaceID string
	Malformed   bool
}

// DatabaseCatalog retains only compact planning metadata, never raw message content.
// A native catalog may need parent IDs before excluding child sessions.
type DatabaseCatalog struct {
	Chats       []DatabaseChat
	NewerFormat int
	Subagents   map[string][]string
}

// DatabaseRecord borrows one indexed native metadata value for its callback.
// No raw value remains valid after the callback returns.
type DatabaseRecord struct {
	Key   string
	Value []byte
}

// DatabaseCatalogHost owns query cursors, row errors and cleanup; the native
// owner chooses its indexed metadata query and interprets each borrowed value.
type DatabaseCatalogHost interface {
	Query(context.Context, string, func(DatabaseRecord) error) error
}

// DatabaseCatalogInspector owns native catalog schema and metadata interpretation.
type DatabaseCatalogInspector interface {
	InspectCatalog(context.Context, DatabaseCatalogHost) (DatabaseCatalog, error)
}

// DatabaseCatalogLookup projects implemented native metadata inventory owners.
type DatabaseCatalogLookup interface {
	LookupDatabaseCatalog(string) (DatabaseCatalogInspector, bool)
}

// CursorRelationshipState records field absence, valid IDs or malformed native routing.
type CursorRelationshipState string

const (
	// CursorRelationshipsAbsent means the producer supplied no relationship field.
	CursorRelationshipsAbsent CursorRelationshipState = "absent"
	// CursorRelationshipsValid means the present child IDs are bounded and valid.
	CursorRelationshipsValid CursorRelationshipState = "valid"
	// CursorRelationshipsMalformed refuses admission while ordinary counting stays lenient.
	CursorRelationshipsMalformed CursorRelationshipState = "malformed"
)

// CursorComposerFacts binds the supported composer codec version and compact
// native relationship evidence across review and admission. It is comparable.
type CursorComposerFacts struct {
	VersionPresent bool
	Version        int
	Relationships  CursorRelationshipState
	ChildIDsSHA256 string
}
