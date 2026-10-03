//go:build !race

package cursorstore

import (
	"runtime"
	"strings"
	"testing"
)

func TestOversizedComposerSignatureAvoidsRawAllocation(t *testing.T) {
	path := StateDatabase(t.TempDir())
	// A scalar unknown field has no effect on the existing equality tuple.
	composer := `{"lastUpdatedAt":7,"conversation":[{"bubbleId":"last"}],"unknown":"` + strings.Repeat("x", 65<<20) + `"}`
	writeDB(t, path, false, map[string]any{"composerData:c": composer})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	sig, err := ReadSignature(t.Context(), path, "c")
	runtime.ReadMemStats(&after)
	if err != nil || sig != (Signature{LastUpdatedAt: 7, HeaderCount: 1, LastBubbleID: "last"}) {
		t.Fatalf("oversized signature: %+v %v", sig, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("signature allocated %d bytes for omitted oversized raw composer", allocated)
	}
}

// Invalid native scalar types must be rejected without materializing their contents.
func TestOversizedInvalidSignatureFieldsAvoidRawAllocation(t *testing.T) {
	// Allocation measurements require a serial test without concurrent fixtures.
	const size = 65 << 20
	large := strings.Repeat("x", size)
	for name, composer := range map[string]string{
		"header object":    `{"fullConversationHeadersOnly":{"bubbleId":"` + large + `"}}`,
		"nested identity":  `{"conversation":[{"bubbleId":"last","unknown":{"bubbleId":"` + large + `"}}],"lastUpdatedAt":"invalid"}`,
		"timestamp string": `{"lastUpdatedAt":"` + large + `"}`,
		"timestamp array":  `{"lastUpdatedAt":["` + large + `"]}`,
		"timestamp object": `{"lastUpdatedAt":{"unknown":"` + large + `"}}`,
		"identity array":   `{"fullConversationHeadersOnly":[{"bubbleId":["` + large + `"]}]}`,
		"identity object":  `{"fullConversationHeadersOnly":[{"bubbleId":{"unknown":"` + large + `"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeHeaders([]byte(composer)); ReasonOf(err) != UnknownFormat {
				t.Fatalf("legacy outcome: %v", err)
			}
			path := StateDatabase(t.TempDir())
			writeDB(t, path, false, map[string]any{"composerData:c": composer})
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := ReadSignature(t.Context(), path, "c")
			runtime.ReadMemStats(&after)
			if ReasonOf(err) != UnknownFormat {
				t.Fatalf("bounded outcome: %v", err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
				t.Fatalf("signature allocated %d bytes for rejected native field", allocated)
			}
		})
	}
}
