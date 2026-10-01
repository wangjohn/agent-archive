package cli

import (
	"bytes"
	"encoding/json"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPairingLedgerMatchesPublishedSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "pairing-ledger.schema.json"))
	must(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	must(t, err)
	id := doc.(map[string]any)["$id"].(string)
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	must(t, c.AddResource(id, doc))
	schema, err := c.Compile(id)
	must(t, err)
	now := time.Now().UTC()
	l := pairingLedger{Version: 1, PairingID: strings.Repeat("a", 32), RecipientID: strings.Repeat("b", 32), IssuerID: strings.Repeat("c", 32), Name: "laptop", DestinationID: "synthetic", Kind: pairingAWSProfile, State: pairingDeliveryIntent, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	data, err := json.Marshal(l)
	must(t, err)
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	must(t, err)
	must(t, schema.Validate(value))
}
