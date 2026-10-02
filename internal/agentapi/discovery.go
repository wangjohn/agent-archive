package agentapi

import (
	"context"
	"io"
	"io/fs"
)

// DiscoveryStage separates filename enumeration from bounded identity inspection.
// Native selection reserves its cumulative header budget between these real stages.
type DiscoveryStage uint8

const (
	DiscoveryReferences DiscoveryStage = iota + 1
	DiscoveryIdentities
)

// DiscoveryFiles supplies narrow read-only native-store operations.
type DiscoveryFiles interface {
	ReadDir(string) ([]fs.DirEntry, error)
	Lstat(string) (fs.FileInfo, error)
	Open(string) (io.ReadCloser, error)
}

// DiscoveryRequest supplies observed locations, purpose and caller-owned limits.
type DiscoveryRequest struct {
	Purpose     DiscoveryPurpose
	Stage       DiscoveryStage
	Locations   NativeLocations
	Roots       []NativeStoreRoot
	Files       DiscoveryFiles
	MaxFiles    int
	HeaderBytes int64
	RecordBytes int64
}

// DiscoveryCandidate contains local evidence, never an admitted registration.
// IdentityError remains local and must not be serialized into diagnostics.
type DiscoveryCandidate struct {
	Session           NativeSession
	Source            SourceRef
	Root              string
	Bytes             int64
	Header            NativeHeader
	IdentityInspected bool
	IdentityError     error
	WorkspaceKey      string
	SourcePriority    int
}

// DiscoveryReport distinguishes incomplete and unreadable stores from empty ones.
type DiscoveryReport struct {
	Enumerated        int
	UnreadableFolders int
	StoreUnreadable   bool
	HistoricalOnly    bool
	Incomplete        bool
}

// Discoverer emits candidates directly into the caller's existing plan or bounded catalog.
type Discoverer interface {
	Discover(context.Context, DiscoveryRequest, func(DiscoveryCandidate) error) (DiscoveryReport, error)
	DefaultDirectories(string) []string
}

// DiscoveryLookup projects implemented enumeration ports without host probes.
type DiscoveryLookup interface {
	LookupDiscovery(string) (Discoverer, bool)
	DiscoveryAgents() []string
}

// NativeDiscoveryLookup supplies only enumeration and verified-header ports to native selection.
type NativeDiscoveryLookup interface {
	DiscoveryLookup
	NativeHeadersLookup
}
