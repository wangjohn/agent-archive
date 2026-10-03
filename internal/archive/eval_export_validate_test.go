package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Model attributes must encode as an object, even when it has no entries.
func TestValidateEvalExportRequiresModelAttributesObject(t *testing.T) {
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
		{name: "populated", attributes: map[string]any{"gen_ai.request.model": "model"}, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, metadata := evalExportFixture(t, "claude")
			raw, err := json.Marshal(EvalExportFromMetadata(metadata, EvalExportSourceArchive))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			model := record["models"].([]any)[0].(map[string]any)
			if tc.omit {
				delete(model, "attributes")
			} else {
				model["attributes"] = tc.attributes
			}
			raw, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			wantValid := tc.valid
			if err := evalExportSchema(t).Validate(instance); (err == nil) != wantValid {
				t.Fatalf("schema validity = %v, want %v: %v", err == nil, wantValid, err)
			}
			var decoded EvalExport
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if err := ValidateEvalExport(decoded); (err == nil) != wantValid {
				t.Errorf("validator validity = %v, want %v: %v", err == nil, wantValid, err)
			}
		})
	}
}

type evalNameField string

const (
	evalModelTokensField evalNameField = "model_tokens"
	evalToolsUsedField   evalNameField = "tools_used"
	evalMCPCallsField    evalNameField = "mcp_calls"
)

// The producer and schema bound names in characters, including supplementary
// Unicode characters, rather than the bytes of their UTF-8 encoding.
func TestValidateEvalExportNameBoundsMatchSchema(t *testing.T) {
	t.Parallel()
	for _, field := range []evalNameField{evalModelTokensField, evalToolsUsedField, evalMCPCallsField} {
		t.Run(string(field), func(t *testing.T) {
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
					_, metadata := evalExportFixture(t, "claude")
					record := EvalExportFromMetadata(metadata, EvalExportSourceArchive)
					switch field {
					case evalModelTokensField:
						record.ModelTokens = []ModelTokens{{Model: tc.text}}
					case evalToolsUsedField:
						record.ToolsUsed = []ToolUsage{{Name: tc.text, Count: 1}}
					case evalMCPCallsField:
						record.MCPCalls = []ToolUsage{{Name: tc.text, Count: 1}}
					}
					raw, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					if err := evalExportSchema(t).Validate(instance); (err == nil) != tc.valid {
						t.Fatalf("schema validity = %v, want %v: %v", err == nil, tc.valid, err)
					}
					if err := ValidateEvalExport(record); (err == nil) != tc.valid {
						t.Errorf("validator validity = %v, want %v: %v", err == nil, tc.valid, err)
					}
				})
			}
		})
	}
}

// Invalid sidecar URLs must fail before the exporter emits a session record.
// Validation follows the actual format assertion, without echoing untrusted URL text.
func TestValidateEvalExportGitURLMatchesSchema(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{"https://github.com/acme/widget/pull/1", "https://github.com/acme/widget/tree/fix%2Fissue", "https://[::1]/acme/widget", "https://example.test/acme/widget?x=a%20b#review", "https://example.test/a%20b", "https://example.test/a b", "https://example.test/a界", "https://example.test", "https://", "http://example.test/private", "git://example.test/private", "https://[::1%25zone]/private", "https://[1:2:3]/private", "https://bad host/private", "https://example.test/%zz", "https://example.test/\nprivate", "https://[not-ip]/private", "https://[::1/private"} {
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			bundle, metadata := evalExportFixture(t, "claude")
			metadata.GitActivity = []GitEvent{{Kind: GitEventPush, Source: GitEventSourceShell, URL: rawURL}}
			for _, detail := range []EvalExportDetail{EvalExportDetailMetadata, EvalExportDetailFull} {
				record, err := BuildEvalExport(bundle, metadata, EvalExportSourceArchive, detail)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				schemaErr := evalExportSchema(t).Validate(instance)
				validationErr := ValidateEvalExport(record)
				if (validationErr == nil) != (schemaErr == nil) {
					t.Errorf("%s: validator = %v, schema = %v", detail, validationErr, schemaErr)
				}
				if validationErr != nil && strings.Contains(validationErr.Error(), rawURL) {
					t.Errorf("%s: invalid URL leaked in validation error", detail)
				}
			}
		})
	}
}
