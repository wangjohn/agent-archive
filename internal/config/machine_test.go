package config

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestMachineAssignmentRoundTripsWithoutSecrets(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := Config{MachineID: strings.Repeat("a", 32), MachineName: "work-laptop", Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}}
	cfg.MachineAssignment = &MachineAssignment{DestinationID: cfg.DestinationID(), Kind: MachineAssignmentR2Own, AccessKeyID: "synthetic-key-id", RecipientID: strings.Repeat("b", 32), IssuerID: strings.Repeat("c", 32)}
	if e := Save(home, cfg); e != nil {
		t.Fatal(e)
	}
	got, found, e := Load(home)
	if e != nil || !found || got.MachineAssignment == nil || got.MachineAssignment.RecipientID != cfg.MachineAssignment.RecipientID {
		t.Fatalf("%+v %v", got, e)
	}
	got.MachineAssignment.RecipientID = "../invalid"
	if e = Save(home, got); e == nil {
		t.Fatal("noncanonical assignment accepted")
	}
}
