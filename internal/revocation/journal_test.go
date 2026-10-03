package revocation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func TestConfirmedOutcomeCannotRegressAndRetryLoadsExactSelection(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("a", 32)
	j := Journal{Version: 1, OperationID: id, DestinationID: strings.Repeat("b", 64), RequesterID: strings.Repeat("c", 32), PermissionID: strings.Repeat("f", 32), AccountID: strings.Repeat("d", 32), Bucket: "synthetic", CreatedAt: time.Now(), Keys: []Key{{ProviderID: id, RecipientID: id, IssuerID: id, SlotID: id, Outcome: Pending}}}
	home := t.TempDir()
	if err := Save(home, j); err != nil {
		t.Fatal(err)
	}
	j.Record(id, Confirmed)
	j.Record(id, Unknown)
	j.Record(id, Pending)
	if !j.Complete() {
		t.Fatal("confirmed deletion regressed")
	}
	if err := Save(home, j); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(home, id)
	if err != nil || !loaded.Complete() || loaded.Keys[0].ProviderID != id {
		t.Fatal("retry selection changed")
	}
	loaded.Keys[0].ProviderID = "secret arbitrary untrusted ID"
	if err := Save(home, loaded); err == nil {
		t.Fatal("unsafe metadata persisted")
	}
}

func TestOperationJournalRejectsOversizedRetryBeforeDecode(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	id := strings.Repeat("a", 32)
	if err := os.MkdirAll(filepath.Join(home, "revocations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home, id), []byte(strings.Repeat(" ", 65537)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home, id); err == nil {
		t.Fatal("oversized journal accepted")
	}
}

func TestPublishedOperationMatchesSecretFreeSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "revocation.schema.json"))
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
	key := strings.Repeat("a", 32)
	j := Journal{Version: 1, OperationID: key, DestinationID: strings.Repeat("b", 64), RequesterID: key, PermissionID: strings.Repeat("f", 32), AccountID: key, Bucket: "synthetic", Jurisdiction: "us", CreatedAt: time.Now().UTC(), Keys: []Key{{ProviderID: key, RecipientID: key, IssuerID: key, SlotID: key, Outcome: Pending}}}
	encoded, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []*RequestedSelector{{Kind: RequestedName, Value: "requested-target"}, {Kind: RequestedMachineID, Value: key}, {Kind: RequestedRecipientID, Value: key}, {Kind: RequestedPairingID, Value: key}} {
		j.RequestedSelector = selector
		if err = j.Validate(); err != nil {
			t.Fatal(err)
		}
		encoded, err = json.Marshal(j)
		if err != nil {
			t.Fatal(err)
		}
		value, err = jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
	value.(map[string]any)["secret_access_key"] = "CANARY"
	if err = schema.Validate(value); err == nil {
		t.Fatal("schema admitted credential values")
	}
}

func TestOperationJournalSelectionBound(t *testing.T) {
	j := Journal{Version: 1, OperationID: strings.Repeat("a", 32), RequesterID: strings.Repeat("b", 32), DestinationID: strings.Repeat("c", 64), PermissionID: strings.Repeat("f", 32), AccountID: strings.Repeat("d", 32), Bucket: "synthetic", CreatedAt: time.Now()}
	for n := range 128 {
		id := fmt.Sprintf("%032x", n)
		j.Keys = append(j.Keys, Key{ProviderID: id, RecipientID: id, IssuerID: id, SlotID: id, Outcome: Pending})
	}
	if err := j.Validate(); err != nil {
		t.Fatal("128 selection rejected", err)
	}
	id := fmt.Sprintf("%032x", 128)
	j.Keys = append(j.Keys, Key{ProviderID: id, RecipientID: id, IssuerID: id, SlotID: id, Outcome: Pending})
	if err := j.Validate(); err == nil {
		t.Fatal("129 selection accepted")
	}
}

func TestRequestedSelectorRejectsUnboundedOrUnknownMetadata(t *testing.T) {
	t.Parallel()
	j := Journal{Version: 1, OperationID: strings.Repeat("a", 32), RequesterID: strings.Repeat("b", 32), DestinationID: strings.Repeat("c", 64), Bucket: "synthetic", CreatedAt: time.Now(), Keys: []Key{}, RequestOnly: true}
	const unknownSelector SelectorKind = "provider_secret"
	for _, selector := range []*RequestedSelector{{Kind: RequestedName, Value: strings.Repeat("a", 41)}, {Kind: RequestedName, Value: "bad\nname"}, {Kind: RequestedRecipientID, Value: "not-an-id"}, {Kind: unknownSelector, Value: "canary"}} {
		j.RequestedSelector = selector
		if err := j.Validate(); err == nil {
			t.Fatalf("unsafe request accepted: %+v", selector)
		}
	}
}
