package nativesessions

import "github.com/wangjohn/agent-archive/internal/discoveryio"

// DirectoryReader supplies ordered native-store enumeration.
type DirectoryReader = discoveryio.DirectoryReader

// StoreRoot declares integration-owned traversal layout.
type StoreRoot = discoveryio.StoreRoot

// Ref is a process-local native transcript reference.
type Ref = discoveryio.Ref

// WalkCoverage distinguishes failed enumeration from an empty store.
type WalkCoverage = discoveryio.WalkCoverage

// Walk shares layout-driven traversal with integration discovery.
var Walk = discoveryio.Walk
