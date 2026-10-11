package state

import (
	"encoding/json"
	"testing"
)

// The size-limit kinds and the signature fields are written to disk and read
// by later versions: their spellings are pinned.
func TestSizeLimitGapSpellings(t *testing.T) {
	t.Parallel()
	for kind, want := range map[SizeLimitKind]string{SizeLimitRaw: "raw", SizeLimitFiltered: "filtered", SizeLimitRecord: "record", SizeLimitReadBudget: "read_budget"} {
		if string(kind) != want {
			t.Errorf("%q, want %q", kind, want)
		}
	}
	encoded, err := json.Marshal(ScanSignature{FailedRawLimit: 7, SizeLimit: &SizeLimitGap{Kind: SizeLimitRaw, LimitBytes: 5, SizeBytes: 7}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	limit, _ := fields["size_limit"].(map[string]any)
	if fields["failed_raw_limit"] != float64(7) || limit["kind"] != "raw" || limit["limit_bytes"] != float64(5) || limit["size_bytes"] != float64(7) {
		t.Fatalf("%s", encoded)
	}
}
