package codex

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"time"
)

// SourceProvider reads verified bounded transcript files.
type SourceProvider struct{}

// Filter delegates the current pure privacy codec behind the native input port.
type Filter struct{ nativecodec.CodexAdapter }

// Filter consumes verified native input under shared collection limits.
func (f Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	if in.Records != nil {
		return filterHistory(ctx, in.Records)
	}
	return sourceio.FilterJSONL(ctx, in, c, nativecodec.FilterCodexCaptureJSONL)
}

// Refilter applies current privacy rules to retained native evidence.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, _ time.Time) (archive.FilteredTranscript, error) {
	if b.History != nil {
		if err := b.ValidateHistory(); err != nil {
			return archive.FilteredTranscript{}, err
		}
		return filterHistory(ctx, &retainedHistory{bundle: b})
	}
	return sourceio.RefilterJSONL(ctx, f, b)
}

// EvidenceExtends compares retained native facts under unchanged codec versions.
func (Filter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	if !sameHistory(previous, candidate) {
		return false
	}
	return nativecodec.EvidenceExtends(previous, candidate)
}
