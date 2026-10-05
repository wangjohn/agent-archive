package agentapi

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type cancelingParser struct {
	cancel context.CancelFunc
	err    error
}

func (p cancelingParser) Version() string { return "synthetic" }

func (p cancelingParser) Parse(context.Context, archive.SourceBundle) (archive.Analysis, error) {
	p.cancel()
	return archive.Analysis{Facts: archive.NativeFacts{Name: "partial"}}, p.err
}

func TestAnalyzeDiscardsLateCanceledResults(t *testing.T) {
	t.Parallel()
	for _, failure := range []error{nil, errors.New("failed native parse"), &archive.ParseError{Reason: "invalid retained evidence"}} {
		ctx, cancel := context.WithCancel(context.Background())
		analysis, err := Analyze(ctx, cancelingParser{cancel: cancel, err: failure}, archive.SourceBundle{})
		cancel()
		if !errors.Is(err, context.Canceled) || analysis.Facts.Name != "" {
			t.Errorf("failure %v: analysis=%#v err=%v", failure, analysis, err)
		}
	}
}
