package discovery

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validatePublishedDiscovery checks the actual consumer wire output against
// published schemas, including discovery provenance in metadata and framing.
func validatePublishedDiscovery(t *testing.T, metadata, source []byte) {
	t.Helper()
	metadataSchema := discoverySchema(t, "metadata.schema.json")
	validateDiscoveryJSON(t, metadataSchema, metadata)
	sourceSchema := discoverySchema(t, "source-bundle.schema.json")
	reader, err := gzip.NewReader(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	scanner := bufio.NewScanner(reader)
	lines := 0
	for scanner.Scan() {
		validateDiscoveryJSON(t, sourceSchema, scanner.Bytes())
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Fatal("publication contained no source framing")
	}
}

func discoverySchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
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
	if err := compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validateDiscoveryJSON(t *testing.T, schema *jsonschema.Schema, raw []byte) {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("published discovery wire violates schema: %v", err)
	}
}
