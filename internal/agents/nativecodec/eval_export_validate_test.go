package nativecodec

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
