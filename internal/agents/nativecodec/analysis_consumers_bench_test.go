package nativecodec

import (
	"context"
	"strings"
	"testing"
	"time"
)

// BenchmarkAnalysisConsumers pins the existing traversal cost when one caller
// asks for labels, metadata, transcript and handoff from the same safe bundle.
func BenchmarkAnalysisConsumers(b *testing.B) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(smallClaudePrompt + "\n" + bigToolResultRecord(1024) + "\n"))
	if err != nil {
		b.Fatal(err)
	}
	bundle, err := NewSourceBundle(registration(), ClaudeAdapter{}, filtered, time.Unix(2, 0), nil)
	if err != nil {
		b.Fatal(err)
	}
	reference := SourceReference{Key: "synthetic", SHA256: strings.Repeat("a", 64)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		analysis, err := ParseClaude(context.Background(), bundle)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := LabelsFromAnalysis(analysis); !ok {
			b.Fatal("labels have no prompt")
		}
		metadata, err := BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic", time.Unix(1, 0), time.Unix(3, 0), reference, ParserInfo{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := BuildTranscriptWithAnalysis(bundle, analysis, HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
		if _, err := BuildHandoffWithAnalysis(bundle, analysis, &metadata, HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
