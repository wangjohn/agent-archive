package agentapi

import "context"

// ChildDiscoveryRequest supplies a qualified parent and read-only native host.
type ChildDiscoveryRequest struct {
	Parent NativeSession
	Source SourceRef
	Files  DiscoveryFiles
}

// ChildCandidate is native child identity and locator evidence before admission.
type ChildCandidate struct {
	NativeID string
	Source   SourceRef
	Bytes    int64
}

// ChildDiscoverer owns native child filename and directory conventions.
type ChildDiscoverer interface {
	DiscoverChildren(context.Context, ChildDiscoveryRequest, func(ChildCandidate) error) (DiscoveryReport, error)
}

// ChildrenLookup projects child discovery only for integrations implementing it.
type ChildrenLookup interface {
	LookupChildren(string) (ChildDiscoverer, bool)
}
