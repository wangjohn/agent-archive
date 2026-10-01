package codex

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Previewer filters bounded complete records without full-session analysis.
type Previewer struct{}

func (Previewer) PreviewRecord(ctx context.Context, record []byte) (archive.RecordPreview, error) {
	return nativecodec.Preview(ctx, nativecodec.CodexAdapter{}, record)
}
