package nativecodec

import (
	"errors"
	"strings"
	"testing"
)

// BenchmarkFilterRecordLimits measures the actual 64 MiB boundary, rather than
// the lowered limit used by refusal unit tests. Fixture construction is untimed.
func BenchmarkFilterRecordLimits(b *testing.B) {
	for _, tc := range []struct {
		name      string
		bulk      int
		oversized bool
	}{
		{"near-limit", MaxRecordBytes - 2048, false},
		{"over-limit", MaxRecordBytes + 1, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			input := smallClaudePrompt + "\n" + bigToolResultRecord(tc.bulk) + "\n"
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for range b.N {
				filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
				if tc.oversized {
					if !errors.Is(err, ErrRecordTooLarge) {
						b.Fatalf("oversized error=%v", err)
					}
				} else if err != nil || len(filtered.Records) != 2 || filtered.Boundary.RetainedBytes > 4096 {
					b.Fatalf("near limit retained=%d records=%d error=%v", filtered.Boundary.RetainedBytes, len(filtered.Records), err)
				}
			}
		})
	}
}
