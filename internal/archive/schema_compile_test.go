package archive

import (
	"bytes"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const schemaDir = "../../schemas"

// compileSchema compiles one published schema, asserting formats
// (date-time) as well as structure.
func compileSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	id, _ := doc.(map[string]any)["$id"].(string)
	if !strings.HasPrefix(id, "https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/"+name) {
		t.Fatalf("%s: $id %q is not the file's raw URL", name, id)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return schema
}
