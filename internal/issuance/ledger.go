// Package issuance records immutable local key lineage without credential values.
// Callers hold issued.lock across read/modify/write and provider operations.
package issuance

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// State describes durable intent. Only Spare is eligible for reservation.
type State string

const (
	CreationIntent State = "creation-intent"
	SecretIntent   State = "secret-intent"
	Spare          State = "spare"
	Reserved       State = "reserved"
	DeliveryIntent State = "delivery-intent"
	Delivered      State = "delivered"
	Own            State = "own"
	CleanupPending State = "cleanup-pending"
	Deleted        State = "deleted"
)

// Origin retains how the issuer first allocated a slot.
type Origin string

const (
	Fresh      Origin = "fresh"
	Precreated Origin = "spare"
	Guided     Origin = "guided"
)

// Slot is trusted local lineage; claims do not mutate its immutable identifiers.
// SecretRef points only at an opaque credential-store entry, never a value.
type Slot struct {
	Version              int       `json:"version"`
	SlotID               string    `json:"slot_id"`
	RecipientID          string    `json:"recipient_id"`
	IssuerID             string    `json:"issuer_id"`
	DestinationID        string    `json:"destination_id"`
	AccountID            string    `json:"account_id"`
	Bucket               string    `json:"bucket"`
	Jurisdiction         string    `json:"jurisdiction,omitempty"`
	PermissionID         string    `json:"permission_id"`
	ProviderID           string    `json:"provider_id,omitempty"`
	ProviderName         string    `json:"provider_name"`
	SecretRef            string    `json:"secret_ref"`
	Origin               Origin    `json:"origin"`
	State                State     `json:"state"`
	PairingID            string    `json:"pairing_id,omitempty"`
	Label                string    `json:"label,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	ExpiresAt            time.Time `json:"expires_at,omitzero"`
	SecretRemovalPending bool      `json:"secret_removal_pending,omitempty"`
	CleanupReason        string    `json:"cleanup_reason,omitempty"`
}

// ProviderName constructs the exact canonical 118-byte provider name.
func ProviderName(recipient, issuer, slot string) string {
	return "agent-archive r=" + recipient + " i=" + issuer + " k=" + slot
}

// New allocates immutable identifiers and a reference before any provider call.
func New(issuer, destination, account string, bucket cloudflare.BucketRef, permission string, origin Origin, now time.Time) (Slot, error) {
	recipient, err := local.ID()
	if err != nil {
		return Slot{}, err
	}
	id, err := local.ID()
	if err != nil {
		return Slot{}, err
	}
	s := Slot{Version: 1, SlotID: id, RecipientID: recipient, IssuerID: issuer, DestinationID: destination, AccountID: account, Bucket: bucket.Name, Jurisdiction: bucket.Jurisdiction, PermissionID: permission, ProviderName: ProviderName(recipient, issuer, id), SecretRef: "issued-" + id, Origin: origin, State: CreationIntent, CreatedAt: now.UTC()}
	return s, s.Validate()
}

// Validate refuses incomplete or ambiguous local binding evidence.
func (s Slot) Validate() error {
	bad := errors.New("invalid issuance ledger; spares withheld")
	for _, id := range []string{s.SlotID, s.RecipientID, s.IssuerID, s.AccountID, s.PermissionID} {
		if !config.ValidMachineID(id) {
			return bad
		}
	}
	if s.Version != 1 || len(s.DestinationID) != 64 || strings.Trim(s.DestinationID, "0123456789abcdef") != "" || s.CreatedAt.IsZero() || (s.SecretRef != "issued-"+s.SlotID && !(s.Origin == Guided && strings.HasPrefix(s.SecretRef, "setup-") && config.ValidMachineID(strings.TrimPrefix(s.SecretRef, "setup-")))) || s.ProviderName != ProviderName(s.RecipientID, s.IssuerID, s.SlotID) {
		return bad
	}
	if _, err := cloudflare.BucketResource(s.AccountID, cloudflare.BucketRef{Name: s.Bucket, Jurisdiction: s.Jurisdiction}); err != nil {
		return bad
	}
	if s.ProviderID != "" && !config.ValidMachineID(s.ProviderID) || s.PairingID != "" && !config.ValidMachineID(s.PairingID) || s.Label != "" && !config.ValidMachineName(s.Label) {
		return bad
	}
	switch s.Origin {
	case Fresh, Precreated, Guided:
	default:
		return bad
	}
	switch s.State {
	case CreationIntent, CleanupPending, Deleted:
	case SecretIntent, Spare, Reserved, DeliveryIntent, Delivered, Own:
		if s.ProviderID == "" {
			return bad
		}
	default:
		return bad
	}
	if (s.State == Reserved || s.State == DeliveryIntent || s.State == Delivered) && (s.PairingID == "" || s.ExpiresAt.IsZero() || s.Label == "") {
		return bad
	}
	if !config.SafeMachineText(s.CleanupReason, 128) {
		return bad
	}
	return nil
}

// Save atomically replaces a private slot record. Caller must hold issued.lock.
func Save(home string, s Slot) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return local.Write(filepath.Join(home, "issued", "slot-"+s.SlotID+".json"), s)
}

// List returns bounded validated lineage, failing closed on corrupt slot entries.
func List(home string) ([]Slot, error) {
	entries, err := os.ReadDir(filepath.Join(home, "issued"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("issuance ledger unreadable; spares withheld")
	}
	if len(entries) > 1000 {
		return nil, errors.New("issuance ledger exceeds entry limit; spares withheld")
	}
	var slots []Slot
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "slot-") {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, errors.New("invalid issuance ledger entry")
		}
		f, err := os.Open(filepath.Join(home, "issued", e.Name()))
		if err != nil {
			return nil, errors.New("issuance ledger unreadable")
		}
		data, err := io.ReadAll(io.LimitReader(f, 16385))
		_ = f.Close()
		var s Slot
		if err != nil || len(data) > 16384 || json.Unmarshal(data, &s) != nil || s.Validate() != nil || e.Name() != "slot-"+s.SlotID+".json" {
			return nil, errors.New("invalid issuance ledger; spares withheld")
		}
		slots = append(slots, s)
	}
	return slots, nil
}
