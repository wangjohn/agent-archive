package pairing

import (
	"bytes"
	"encoding/json"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"path/filepath"
	"testing"
)

func TestPayloadMatchesPublishedSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "pairing-payload.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	id := doc.(map[string]any)["$id"].(string)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err = compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(testPayload())
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(value); err != nil {
		t.Fatal(err)
	}
}
