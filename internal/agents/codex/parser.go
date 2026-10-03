package codex

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives common analysis from retained codex source without host effects.
type Parser struct{}

// Version identifies the native derivation policy.
func (Parser) Version() string { return archive.DefaultParserVersion }

// Parse derives facts once from retained safe evidence.
func (Parser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	return nativecodec.ParseCodex(ctx, bundle)
}
