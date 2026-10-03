package nativesessions

import "github.com/wangjohn/agent-archive/internal/discoveryio"

// DirectoryReader supplies ordered native-store enumeration.
type DirectoryReader = discoveryio.DirectoryReader

// StoreRoot declares integration-owned traversal layout.
type StoreRoot = discoveryio.StoreRoot

// Ref is a process-local native transcript reference.
type Ref = discoveryio.Ref
