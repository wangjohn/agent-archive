package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
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

func validateAgainst(t *testing.T, schema *jsonschema.Schema, what string, data []byte) {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Errorf("%s does not match its schema: %v\n%s", what, err, data)
	}
}

// Every fixture's source bundle, line by line, and its metadata sidecar (as
// hook-captured and as imported) validate against the published schemas.
// This is what keeps a schema from drifting from what the code writes.
func TestFixtureOutputMatchesPublishedSchemas(t *testing.T) {
	sourceSchema := compileSchema(t, "source-bundle.schema.json")
	metadataSchema := compileSchema(t, "metadata.schema.json")
	derived := time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
	bundles := fixtureBundles(t)
	// A skill invocation, so skills_used is exercised as well.
	bundles["claude-skill-invocation"] = claudeLines(t,
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"review it"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"review-pr"}}]}}`,
	)
	exercised := map[string]bool{}
	for name, bundle := range bundles {
		var encoded bytes.Buffer
		if err := EncodeSource(&encoded, bundle); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		scanner := bufio.NewScanner(&encoded)
		scanner.Buffer(nil, 1<<26)
		for line := 1; scanner.Scan(); line++ {
			validateAgainst(t, sourceSchema, name+" source line "+strconv.Itoa(line), scanner.Bytes())
		}
		for _, imported := range []bool{false, true} {
			metadata, err := BuildMetadata(bundle, "machine-1", bundle.Capture.CapturedAt.Add(-time.Hour), derived,
				SourceReference{Key: "sessions/x/y/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 10}, ParserInfo{})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if imported {
				reg := registration()
				reg.Origin, reg.AdmittedAt, reg.StartedAtSource = SessionOriginImport, derived, StartedAtSourceCursorComposer
				metadata.ApplyRegistrationProvenance(reg)
			}
			data, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			validateAgainst(t, metadataSchema, name+" metadata", data)
			exercised["models"] = exercised["models"] || len(metadata.Models) > 0
			exercised["skills_used"] = exercised["skills_used"] || len(metadata.SkillsUsed) > 0
			exercised["capture_gaps"] = exercised["capture_gaps"] || len(metadata.CaptureGaps) > 0
			exercised["started_at_source"] = exercised["started_at_source"] || metadata.StartedAtSource != ""
			for _, gap := range metadata.CaptureGaps {
				if !slices.Contains(CaptureGapCodes, gap.Code) {
					t.Errorf("%s: gap code %q is not in CaptureGapCodes", name, gap.Code)
				}
			}
		}
	}
	checkExercised(t, exercised)
}

// checkExercised fails when no fixture produced one of the typed metadata
// arrays, which would leave that part of the schema untested.
func checkExercised(t *testing.T, exercised map[string]bool) {
	t.Helper()
	for _, field := range []string{"models", "skills_used", "capture_gaps", "started_at_source"} {
		if !exercised[field] {
			t.Errorf("no fixture metadata has %s, so its schema is untested", field)
		}
	}
}

// schemaEnum returns the enum at a JSON pointer-like path of properties.
func schemaEnum(t *testing.T, name string, path ...string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(schemaDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	for _, key := range path {
		next, ok := node.(map[string]any)[key]
		if !ok {
			t.Fatalf("%s: no %s in %v", name, key, path)
		}
		node = next
	}
	var values []string
	for _, value := range node.(map[string]any)["enum"].([]any) {
		values = append(values, value.(string))
	}
	sort.Strings(values)
	return values
}

// goEnum returns the values of every constant of the named string type in
// this package's non-test source.
func goEnum(t *testing.T, typeName string) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	set := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(set, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				ident, ok := value.Type.(*ast.Ident)
				if !ok || ident.Name != typeName {
					continue
				}
				for _, v := range value.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						unquoted, _ := strconv.Unquote(lit.Value)
						values = append(values, unquoted)
					}
				}
			}
		}
	}
	sort.Strings(values)
	return values
}

// The schemas' enums are the Go constants, value for value.
func TestSchemaEnumsMatchGoConstants(t *testing.T) {
	for _, tc := range []struct {
		goType string
		schema string
		path   []string
	}{
		{"ParserStatus", "metadata.schema.json", []string{"properties", "parser", "properties", "status"}},
		{"MetadataState", "metadata.schema.json", []string{"properties", "state"}},
		{"TurnOutcome", "metadata.schema.json", []string{"properties", "turn_outcome"}},
		{"SkillDetection", "metadata.schema.json", []string{"properties", "skill_detection"}},
		{"StartedAtSource", "metadata.schema.json", []string{"properties", "started_at_source"}},
		{"SkillCoverage", "metadata.schema.json", []string{"$defs", "skill", "properties", "coverage"}},
		{"SkillUseEvidence", "metadata.schema.json", []string{"$defs", "skill_use", "properties", "evidence"}},
		{"ModelSummarySource", "metadata.schema.json", []string{"$defs", "model", "properties", "source"}},
		{"ResponseModelStatus", "metadata.schema.json", []string{"$defs", "model", "properties", "response_model_status"}},
		{"LinkedSessionStatus", "metadata.schema.json", []string{"$defs", "linked_session", "properties", "status"}},
		{"LinkedSessionStatus", "source-bundle.schema.json", []string{"$defs", "linked_session", "properties", "status"}},
	} {
		want, got := goEnum(t, tc.goType), schemaEnum(t, tc.schema, tc.path...)
		if len(want) == 0 || !slices.Equal(want, got) {
			t.Errorf("%s in %s: schema %v, Go %v", tc.goType, tc.schema, got, want)
		}
	}
}

var gapCodeLiteral = regexp.MustCompile(`(?:addGap\(|Code:\s*)"([a-z_]+)"`)

// Both schemas enumerate exactly CaptureGapCodes, and every gap code this
// package writes as a literal is in it.
func TestCaptureGapCodesAreEnumerated(t *testing.T) {
	want := append([]string(nil), CaptureGapCodes...)
	sort.Strings(want)
	for _, name := range []string{"metadata.schema.json", "source-bundle.schema.json"} {
		raw, _ := os.ReadFile(filepath.Join(schemaDir, name))
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		anyOf := schema["$defs"].(map[string]any)["gap_code"].(map[string]any)["anyOf"].([]any)
		var got []string
		for _, code := range anyOf[0].(map[string]any)["enum"].([]any) {
			got = append(got, code.(string))
		}
		sort.Strings(got)
		if !slices.Equal(want, got) {
			t.Errorf("%s gap codes %v, want %v", name, got, want)
		}
	}
	files, _ := filepath.Glob("*.go")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range gapCodeLiteral.FindAllStringSubmatch(string(source), -1) {
			if !slices.Contains(CaptureGapCodes, match[1]) {
				t.Errorf("%s writes gap code %q, which CaptureGapCodes does not list", file, match[1])
			}
		}
	}
}
