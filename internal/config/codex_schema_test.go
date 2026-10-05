package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSavedCodexCapturePolicyMatchesLocalSchema(t *testing.T) {
	c, _ := blanketConfig(t)
	raw, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	value, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	schemaRaw, e := os.ReadFile(filepath.Join("..", "..", "schemas", "codex-capture.schema.json"))
	if e != nil {
		t.Fatal(e)
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(schemaRaw))
	if e != nil {
		t.Fatal(e)
	}
	id := doc.(map[string]any)["$id"].(string)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if e = compiler.AddResource(id, doc); e != nil {
		t.Fatal(e)
	}
	schema, e := compiler.Compile(id)
	if e != nil {
		t.Fatal(e)
	}
	if e = schema.Validate(value.(map[string]any)["codex_capture"]); e != nil {
		t.Fatal(e)
	}
}
