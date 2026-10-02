package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// analyzeSource resolves the pure parser once at a read operation boundary.
func analyzeSource(ctx context.Context, parsers agentapi.ParsersLookup, bundle archive.SourceBundle) (archive.Analysis, error) {
	if parsers == nil {
		return archive.Analysis{}, &archive.ParseError{Reason: "parser unavailable"}
	}
	parser, ok := parsers.LookupParser(bundle.Capture.Harness.Name)
	if !ok {
		return archive.Analysis{}, &archive.ParseError{Reason: "parser unavailable"}
	}
	return agentapi.Analyze(ctx, parser, bundle)
}
func analyzeTarget(ctx context.Context, parsers agentapi.ParsersLookup, target handoffTarget) (handoffTarget, error) {
	target.analysis, target.parseErr = analyzeSource(ctx, parsers, target.bundle)
	target.analyzed = true
	return target, target.parseErr
}
