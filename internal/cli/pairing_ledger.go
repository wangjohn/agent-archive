package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
)

type pairingCredentialKind string

const (
	pairingOwnR2      pairingCredentialKind = "r2_own"
	pairingSharedR2   pairingCredentialKind = "r2_shared"
	pairingAWSProfile pairingCredentialKind = "aws_profile"
)

type pairingDeliveryState string

const (
	pairingPrepared       pairingDeliveryState = "prepared"
	pairingDeliveryIntent pairingDeliveryState = "delivery-intent"
	pairingDelivered      pairingDeliveryState = "delivered"
	pairingClaimObserved  pairingDeliveryState = "claim-observed"
	pairingExpired        pairingDeliveryState = "expired"
	pairingCancelled      pairingDeliveryState = "cancelled"
)

// pairingLedger deliberately contains no code, bundle, payload, or secret.
// A delivery-intent written before exposure stays uncertain after interruption.
type pairingLedger struct {
	SlotID            string                `json:"slot_id,omitempty"`
	Version           int                   `json:"version"`
	PairingID         string                `json:"pairing_id"`
	RecipientID       string                `json:"recipient_id"`
	IssuerID          string                `json:"issuer_id"`
	Name              string                `json:"name"`
	DestinationID     string                `json:"destination_id"`
	AccessKeyID       string                `json:"access_key_id,omitempty"`
	CredentialRef     string                `json:"credential_ref,omitempty"`
	Kind              pairingCredentialKind `json:"kind"`
	State             pairingDeliveryState  `json:"state"`
	CreatedAt         time.Time             `json:"created_at"`
	ExpiresAt         time.Time             `json:"expires_at"`
	DeliveredAt       time.Time             `json:"delivered_at,omitzero"`
	ObservedMachineID string                `json:"observed_machine_id,omitempty"`
}

func (l pairingLedger) valid() bool {
	return (l.Kind != pairingOwnR2 || pairing.ValidID(l.SlotID)) && l.Version == 1 && pairing.ValidID(l.PairingID) && pairing.ValidID(l.RecipientID) && pairing.ValidID(l.IssuerID) && pairing.ValidName(l.Name) && l.DestinationID != "" && len(l.DestinationID) <= 128 && len(l.AccessKeyID) <= 128 && len(l.CredentialRef) <= 128 && slices.Contains([]pairingCredentialKind{pairingSharedR2, pairingAWSProfile, pairingOwnR2}, l.Kind) && slices.Contains([]pairingDeliveryState{pairingPrepared, pairingDeliveryIntent, pairingDelivered, pairingClaimObserved, pairingExpired, pairingCancelled}, l.State) && (l.ObservedMachineID == "" || pairing.ValidID(l.ObservedMachineID))
}

func savePairingLedger(home string, l pairingLedger) error {
	if !l.valid() {
		return fmt.Errorf("invalid pairing ledger state")
	}
	return local.Write(filepath.Join(home, "issued", l.PairingID+".json"), l)
}

func readPairingLedgers(home string) ([]pairingLedger, error) {
	entries, err := os.ReadDir(filepath.Join(home, "issued"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pairing ledger could not be read")
	}
	if len(entries) > 1000 {
		return nil, fmt.Errorf("pairing ledger exceeds the 1000-entry limit")
	}
	var ledgers []pairingLedger
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "slot-") {
			continue
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			continue
		}
		f, err := os.Open(filepath.Join(home, "issued", entry.Name()))
		if err != nil {
			return ledgers, fmt.Errorf("a pairing ledger entry could not be read")
		}
		data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
		_ = f.Close()
		var l pairingLedger
		if err != nil || len(data) > 16*1024 || json.Unmarshal(data, &l) != nil || !l.valid() || entry.Name() != l.PairingID+".json" {
			return ledgers, fmt.Errorf("a pairing ledger entry is invalid")
		}
		ledgers = append(ledgers, l)
	}
	return ledgers, nil
}

func pairingWarnings(home string, now time.Time) []string {
	ledgers, err := readPairingLedgers(home)
	if err != nil {
		return []string{err.Error()}
	}
	var warnings []string
	for _, l := range ledgers {
		if l.State == pairingClaimObserved || l.State == pairingCancelled {
			continue
		}
		if now.After(l.ExpiresAt) {
			warnings = append(warnings, fmt.Sprintf("Pairing %s for %s expired; claim not observed. Access may still work; revoke a dedicated key explicitly or replace a shared R2 key everywhere.", l.PairingID, l.Name))
		} else if l.State == pairingDeliveryIntent {
			warnings = append(warnings, fmt.Sprintf("Pairing %s for %s has uncertain delivery; its key remains valid. Claim not observed.", l.PairingID, l.Name))
		} else {
			warnings = append(warnings, fmt.Sprintf("Pairing %s for %s is pending; claim not observed.", l.PairingID, l.Name))
		}
	}
	return append(warnings, dedicatedWarnings(home)...)
}

// Bucket claims are informational. Only matching destination, pairing and recipient
// metadata advances a delivered claim; failed/partial listings never erase state.
func observePairingClaims(home, destination string, result machines.ListResult, now time.Time) {
	initial, e := readPairingLedgers(home)
	if e != nil || len(initial) == 0 {
		return
	}
	release, err := local.NamedLockWait(home, "issued.lock", 10*time.Millisecond)
	if err != nil {
		return
	}
	defer release()
	ledgers, err := readPairingLedgers(home)
	if err != nil {
		return
	}
	for _, l := range ledgers {
		if l.DestinationID != destination || l.State == pairingCancelled || l.State == pairingClaimObserved {
			continue
		}
		old := l.State
		for _, record := range result.Records {
			if record.PairingID == l.PairingID && record.Credential.RecipientID == l.RecipientID && record.Credential.IssuerID == l.IssuerID && record.Credential.SlotID == l.SlotID && string(record.Credential.Kind) == string(l.Kind) && record.Credential.AccessKeyID == l.AccessKeyID && record.PairedFrom == l.IssuerID {
				l.State = pairingClaimObserved
				l.ObservedMachineID = record.MachineID
				break
			}
		}
		if l.State != pairingClaimObserved && now.After(l.ExpiresAt) {
			l.State = pairingExpired
		}
		if old != l.State {
			_ = savePairingLedger(home, l)
		}
	}
}
