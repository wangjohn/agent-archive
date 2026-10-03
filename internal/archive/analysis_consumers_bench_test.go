package archive

import (
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
		if _, ok := SessionLabels(bundle); !ok {
			b.Fatal("labels have no prompt")
		}
		metadata, err := BuildMetadata(bundle, "synthetic", time.Unix(1, 0), time.Unix(3, 0), reference, ParserInfo{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := BuildTranscript(bundle, HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
		if _, err := BuildHandoff(bundle, &metadata, HandoffOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
