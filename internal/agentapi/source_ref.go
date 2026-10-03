package agentapi

import "github.com/wangjohn/agent-archive/internal/archive"

// SourceRef is a relocatable local locator, separate from session identity.
type SourceRef struct {
	Kind archive.SourceKind
	Path string
	Key  string
}
