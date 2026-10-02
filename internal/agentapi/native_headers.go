package agentapi

import "time"

// DiscoveryPurpose separates compatibility import headers from verified native selection.
type DiscoveryPurpose uint8

const (
	DiscoveryImport DiscoveryPurpose = iota + 1
	DiscoveryHandoff
	DiscoveryProjects
)

// NativeHeader contains only inspected native identity and checkout evidence.
type NativeHeader struct {
	NativeID         string
	Directory        string
	StartedAt        time.Time
	IdentityMismatch bool
	SubagentOnly     bool
}

// NativeHeaderRequest supplies a caller-owned bounded scan; codecs cannot open paths.
type NativeHeaderRequest struct {
	Purpose DiscoveryPurpose
	Path    string
	Scan    func(func([]byte) bool) error
}

// NativeHeaderInspector interprets native headers without host operations.
// NativeStoreRoot declares native traversal layout for one purpose.
type NativeStoreRoot struct {
	Priority   int
	Historical bool
	Harness    string
	Path       string
	Recursive  bool
	Depth      int
	Prefix     string
	Suffix     string
}

// NativeLocations supplies observed native configuration directories.
type NativeLocations struct {
	UserHome    string
	Directories []string
}

type NativeHeaderInspector interface {
	Roots(NativeLocations, DiscoveryPurpose) []NativeStoreRoot
	InspectHeader(NativeHeaderRequest) (NativeHeader, error)
}

// NativeHeadersLookup projects native header inspection to read-only consumers.
type NativeHeadersLookup interface {
	LookupNativeHeaders(string) (NativeHeaderInspector, bool)
	NativeHeaderAgents() []string
}
