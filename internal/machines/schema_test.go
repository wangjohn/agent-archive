package machines

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestMachineRecordMatchesPublishedSchema(t *testing.T) {
	t.Parallel()
	raw, e := os.ReadFile(filepath.Join("..", "..", "schemas", "machine.schema.json"))
	if e != nil {
		t.Fatal(e)
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
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
	r := sample(t, strings.Repeat("a", 32))
	r.UnusedSpares = []CredentialBinding{{Kind: config.MachineAssignmentR2Own, AccessKeyID: strings.Repeat("b", 32), RecipientID: strings.Repeat("c", 32), IssuerID: r.MachineID, SlotID: strings.Repeat("d", 32)}}
	r.RetiredCredentials = []CredentialBinding{{Kind: config.MachineAssignmentR2Own, AccessKeyID: strings.Repeat("e", 32), RecipientID: strings.Repeat("f", 32), IssuerID: r.MachineID, SlotID: strings.Repeat("1", 32)}}
	r.CredentialHistoryPartial = true
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	value, e := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if e != nil {
		t.Fatal(e)
	}
	if e = schema.Validate(value); e != nil {
		t.Fatal(e)
	}
}
