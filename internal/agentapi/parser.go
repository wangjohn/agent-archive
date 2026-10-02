package agentapi

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// TranscriptParser interprets only retained safe evidence and performs no I/O.
type TranscriptParser interface {
	Version() string
	Parse(context.Context, archive.SourceBundle) (archive.Analysis, error)
}

// ParsersLookup supplies only the parser needed by a derivation operation.
type ParsersLookup interface {
	LookupParser(name string) (TranscriptParser, bool)
}

// RecordPreviewer filters one bounded complete native record into safe facts.
// It does not establish whole-source identity or completeness.
type RecordPreviewer interface {
	PreviewRecord(context.Context, []byte) (archive.RecordPreview, error)
}

// PreviewsLookup supplies a bounded preview operation without a full parser.
type PreviewsLookup interface {
	LookupPreview(name string) (RecordPreviewer, bool)
}

// RetainedComparator interprets native mutable labels and synthetic metadata.
// Shared orchestration retains the rewrite/publication decision.
type RetainedComparator interface {
	EvidenceExtends(previous, candidate archive.SourceBundle) bool
}

// Analyze keeps parse failures distinct from cancellation so orchestration can
// retain safe filtered evidence when an injected parser cannot derive facts.
func Analyze(ctx context.Context, parser TranscriptParser, bundle archive.SourceBundle) (archive.Analysis, error) {
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	if parser == nil {
		return archive.Analysis{}, &archive.ParseError{Reason: "parser unavailable"}
	}
	analysis, err := parser.Parse(ctx, bundle)
	if err == nil {
		if err := ctx.Err(); err != nil {
			return archive.Analysis{}, err
		}
		return analysis, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return archive.Analysis{}, err
	}
	if archive.IsParseError(err) {
		return archive.Analysis{}, err
	}
	return archive.Analysis{}, &archive.ParseError{Reason: err.Error()}
}
