package nativecodec_test

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"
	"time"
)

// BenchmarkAnalysisConsumers pins the existing traversal cost when one caller
// asks for labels, metadata, transcript and handoff from the same safe bundle.
func BenchmarkAnalysisConsumers(b *testing.B) {
	filtered, err := (claude.Filter{}).FilterJSONL(strings.NewReader(analysisPrompt + "\n" + analysisResult(1024) + "\n"))
	if err != nil {
		b.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(analysisRegistration(), claude.Filter{}, filtered, time.Unix(2, 0), nil)
	if err != nil {
		b.Fatal(err)
	}
	reference := archive.SourceReference{Key: "synthetic", SHA256: strings.Repeat("a", 64)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		analysis, err := agentapi.Analyze(context.Background(), claude.Parser{}, bundle)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := archive.LabelsFromAnalysis(analysis); !ok {
			b.Fatal("labels have no prompt")
		}
		metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic", time.Unix(1, 0), time.Unix(3, 0), reference, archive.ParserInfo{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := archive.BuildTranscriptWithAnalysis(bundle, analysis, archive.HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
		if _, err := archive.BuildHandoffWithAnalysis(bundle, analysis, &metadata, archive.HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func analysisResult(bulkBytes int) string {
	bulk := strings.Repeat("x", bulkBytes)
	return fmt.Sprintf(`{"type":"user","uuid":"r1","sessionId":"native-claude","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"short summary of the output"}]},"toolUseResult":{"stdout":%q,"stderr":""}}`, bulk)
}

const analysisPrompt = `{"type":"user","uuid":"p1","sessionId":"native-claude","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":"Run the tests."}}`

func analysisRegistration() archive.SessionRegistration {
	return archive.SessionRegistration{
		ArchiveSessionID: "archive-123", NativeSessionID: "native-456", ProjectID: "project-789", ProjectRoot: "/work/widget",
		Harness: archive.Harness{Name: "codex", Version: "observed-build", Mode: "desktop"}, TranscriptPath: "/private/log.jsonl",
		SessionStartedAt: time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 9, 17, 18, 1, 0, 0, time.UTC),
	}
}
