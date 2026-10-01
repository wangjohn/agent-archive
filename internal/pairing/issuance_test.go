package pairing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestDedicatedPayloadStrictJSONProvenance(t *testing.T) {
	t.Parallel()
	p := testPayload()
	p.Kind = config.MachineAssignmentR2Own
	p.SlotID = strings.Repeat("5", 32)
	p.AccessKeyID = strings.Repeat("6", 32)
	p.Storage.Provider = "r2"
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = payloadJSON(data); err != nil {
		t.Fatal(err)
	}
	if err = p.validateProvenance(); err != nil {
		t.Fatal(err)
	}
	for _, replace := range []struct{ before, after string }{
		{`"kind":`, `"Kind":`},
		{`"slot_id":`, `"Slot_ID":`},
		{`"kind":"r2_own"`, `"kind":null`},
		{`"slot_id":"` + p.SlotID + `"`, `"slot_id":null`},
	} {
		if payloadJSON([]byte(strings.Replace(string(data), replace.before, replace.after, 1))) == nil {
			t.Fatal("noncanonical dedicated field accepted")
		}
	}
	p.SlotID = ""
	if p.validateProvenance() == nil {
		t.Fatal("own payload without slot accepted")
	}
	p.Kind = ""
	if p.validateProvenance() != nil {
		t.Fatal("legacy shared provenance rejected")
	}
	p.SlotID = strings.Repeat("5", 32)
	if p.validateProvenance() == nil {
		t.Fatal("legacy shared payload claimed dedicated slot")
	}
}
