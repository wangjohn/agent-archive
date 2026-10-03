package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func rewriteEvalSidecar(t *testing.T, edit func(map[string]any)) (Env, string) {
	t.Helper()
	env, store, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar map[string]any
	if err := json.Unmarshal(raw, &sidecar); err != nil {
		t.Fatal(err)
	}
	edit(sidecar)
	raw, err = json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, raw); err != nil {
		t.Fatal(err)
	}
	return env, id
}

func TestEvalExportRequiresModelAttributesAtBothDetails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		attributes map[string]any
		omit       bool
		valid      bool
	}{
		{name: "missing", omit: true},
		{name: "null"},
		{name: "empty", attributes: map[string]any{}, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, id := rewriteEvalSidecar(t, func(sidecar map[string]any) {
				model := map[string]any{"source": "native_transcript", "response_model_status": "observed"}
				if !tc.omit {
					model["attributes"] = tc.attributes
				}
				sidecar["models"] = []any{model}
			})
			for _, detail := range []string{"metadata", "full"} {
				records, stderr, code := evalLines(t, env, "--detail", detail, id)
				if len(records) != 1 || stderr != "" {
					t.Fatalf("%s: records %v stderr %q", detail, records, stderr)
				}
				if tc.valid {
					if code != 0 || records[0]["record"] != "session" {
						t.Errorf("%s: exit %d, records %v", detail, code, records)
					}
				} else if code != 1 || records[0]["record"] != "error" || records[0]["error"].(map[string]any)["code"] != "read_failed" {
					t.Errorf("%s: exit %d, records %v", detail, code, records)
				}
			}
		})
	}
}

func TestEvalExportNameBoundsAtBothDetails(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"model_tokens", "tools_used", "mcp_calls"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				text  string
				valid bool
			}{
				{"empty", "", false},
				{"ascii128", strings.Repeat("a", 128), true},
				{"ascii129", strings.Repeat("a", 129), false},
				{"unicode128", strings.Repeat("界😀", 64), true},
				{"unicode129", strings.Repeat("界😀", 64) + "界", false},
				{"controls128", strings.Repeat("\u009b", 128), true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					env, id := rewriteEvalSidecar(t, func(sidecar map[string]any) {
						if field == "model_tokens" {
							sidecar[field] = []any{map[string]any{"model": tc.text}}
						} else {
							sidecar[field] = []any{map[string]any{"name": tc.text, "count": 1}}
						}
					})
					for _, detail := range []string{"metadata", "full"} {
						records, stderr, code := evalLines(t, env, "--detail", detail, id)
						if len(records) != 1 || stderr != "" {
							t.Fatalf("%s: records %v stderr %q", detail, records, stderr)
						}
						if !tc.valid {
							if code != 1 || records[0]["record"] != "error" || records[0]["error"].(map[string]any)["code"] != "read_failed" {
								t.Errorf("%s: exit %d, records %v", detail, code, records)
							}
							continue
						}
						if code != 0 || records[0]["record"] != "session" {
							t.Errorf("%s: exit %d, records %v", detail, code, records)
							continue
						}
						key := "name"
						if field == "model_tokens" {
							key = "model"
						}
						if got := records[0][field].([]any)[0].(map[string]any)[key]; got != tc.text {
							t.Errorf("%s: name %q, want %q", detail, got, tc.text)
						}
					}
				})
			}
		})
	}
}
