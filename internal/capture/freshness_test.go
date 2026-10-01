package capture

import (
	"os"
	"path/filepath"
	"testing"
)

// Native source enums override file observations only for Claude and Codex.
// This table pins the distinction between missing, null, mistyped and empty
// fields before lifecycle decoding turns the predicate into typed evidence.
func TestFreshnessEvidenceFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty, nonempty := filepath.Join(dir, "empty"), filepath.Join(dir, "conversation")
	for path, content := range map[string]string{empty: "", nonempty: "existing"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unreadable := filepath.Join(dir, "mode-000-empty")
	if err := os.WriteFile(unreadable, nil, 0000); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []string{"claude", "codex", "cursor"} {
		for _, tc := range []struct {
			name   string
			source any
			path   any
			omit   bool
			want   bool
		}{
			{"startup with existing", " STARTUP ", nonempty, false, harness != "cursor"},
			{"clear with existing", "clear", nonempty, false, harness != "cursor"},
			{"resume with empty", "resume", empty, false, harness == "cursor"},
			{"compact with empty", "compact", empty, false, harness == "cursor"},
			{"unknown source with empty", "other", empty, false, harness == "cursor"},
			{"mistyped source with empty", 42, empty, false, true},
			{"whitespace source with empty", " \t\n ", empty, false, true},
			{"mode 000 empty file", nil, unreadable, false, true},
			{"missing transcript", nil, filepath.Join(dir, "missing"), false, true},
			{"nonempty transcript", nil, nonempty, false, false},
			{"relative transcript", nil, "missing.jsonl", false, false},
			{"directory", nil, dir, false, false},
			{"null path", nil, nil, false, harness == "cursor"},
			{"absent path", nil, nil, true, harness == "cursor"},
			{"empty path", nil, "", false, harness == "cursor"},
			{"mistyped path", nil, 42, false, false},
		} {
			t.Run(harness+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				payload := map[string]any{"source": tc.source}
				if !tc.omit {
					payload["transcript_path"] = tc.path
				}
				if got := provesFreshSessionStart(harness, payload); got != tc.want {
					t.Fatalf("fresh=%v, want %v", got, tc.want)
				}
			})
		}
	}
}
