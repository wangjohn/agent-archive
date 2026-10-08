package state

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestCatalogPendingCommitRoundTripAndMalformedRecovery(t *testing.T) {
	local := newTestStore(t)
	source := []byte("private source")
	pending := PendingPublication{Catalog: &CatalogPublication{ID: "frozen", ExpectedRevision: "previous"}, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: durableRef(source).SHA256, SourceBytes: source, MetadataBytes: []byte(`{}`)}
	if err := local.SavePending("catalog", pending); err != nil {
		t.Fatal(err)
	}
	got, found, err := local.LoadPending("catalog")
	if err != nil || !found || got.Catalog == nil || *got.Catalog != *pending.Catalog {
		t.Fatal("commit roundtrip failed", err)
	}
	raw, err := os.ReadFile(local.pendingPath("catalog"))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	// The commit spelling is intentionally an old durable writer refusal field.
	if wire["commit"] == nil || wire["catalog"] != nil {
		t.Fatal("old writer compatibility fence missing")
	}
	for _, foreign := range []string{`{}`, `{"id":"frozen","phase":"future"}`, `{"id":1}`} {
		wire["commit"] = json.RawMessage(foreign)
		bad, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var decoded PendingPublication
		if err = json.Unmarshal(bad, &decoded); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("foreign commit accepted", foreign, err)
		}
	}
}
