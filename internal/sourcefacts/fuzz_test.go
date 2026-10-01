package sourcefacts

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzCodexHeader checks the preauthorization privacy and resource boundary.
func FuzzCodexHeader(f *testing.F) {
	f.Add("{")
	f.Add(`{"type":"session_meta","payload":{"id":"00000000-0000-0000-0000-000000000001","timestamp":"2026-10-01T12:00:00Z","cwd":"/included","source":"cli","originator":"synthetic","cli_version":"test"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"00000000-0000-0000-0000-000000000001","root_turn_id":"00000000-0000-0000-0000-000000000001","started_at":1790856000}}` + "\n")
	f.Fuzz(func(t *testing.T, input string) {
		h := ReadCodexHeader(strings.NewReader(input), "rollout-"+testID+".jsonl")
		if h.Bytes > HeaderBytes {
			t.Fatal("read budget exceeded")
		}
		if h.Outcome != "native_format" {
			if !reflect.DeepEqual(h.Meta, CodexMeta{}) || !h.Started.IsZero() {
				t.Fatal("rejected source retained metadata")
			}
		} else {
			// Identity/cwd/producer limits keep a retained record small regardless of
			// arbitrary body content or unknown fields in the input.
			encoded, err := json.Marshal(h)
			if err != nil || len(encoded) > 8192 {
				t.Fatal("unbounded retained metadata")
			}
		}
	})
}
