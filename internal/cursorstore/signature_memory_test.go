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
