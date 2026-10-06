package nativecodec

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCodexLabelHistoryModeKeepsOnlyNativeEnum(t *testing.T) {
	t.Parallel()
	for _, value := range []any{"legacy", "paginated", "private arbitrary mode", true, 42, map[string]any{"text": "private metadata"}} {
		raw, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": "01900000-0000-7000-8000-000000000001", "cli_version": "0.159.2", "history_mode": value}})
		if err != nil {
			t.Fatal(err)
		}
		filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(append(raw, '\n')))
		if err != nil {
			t.Fatal(err)
		}
		payload := child(t, decodeRecords(t, filtered)[0], "payload")
		mode, kept := payload["history_mode"]
		text, isString := value.(string)
		want := isString && (text == "legacy" || text == "paginated")
		if kept != want || (want && mode != value) {
			t.Fatalf("history mode retained wrong typed shape: kept=%t want=%t", kept, want)
		}
	}
}
