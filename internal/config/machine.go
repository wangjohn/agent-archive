package config

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MachineAssignment is nonsecret, locally trusted provenance. Bucket records
// cannot replace it or authorize management operations.
type MachineAssignment struct {
	DestinationID string     `json:"destination_id"`
	Kind          string     `json:"kind"`
	AccessKeyID   string     `json:"access_key_id,omitempty"`
	RecipientID   string     `json:"recipient_id,omitempty"`
	IssuerID      string     `json:"issuer_id,omitempty"`
	SlotID        string     `json:"slot_id,omitempty"`
	SharedWith    string     `json:"shared_with,omitempty"`
	PairingID     string     `json:"pairing_id,omitempty"`
	PairedFrom    string     `json:"paired_from,omitempty"`
	PairedAt      *time.Time `json:"paired_at,omitempty"`
}

var machineIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidMachineID reports whether an identifier is canonical lowercase 128-bit hex.
func ValidMachineID(s string) bool { return machineIDPattern.MatchString(s) }

// ValidMachineName reports whether a chosen machine label is safe and bounded.
func ValidMachineName(s string) bool { return machineNamePattern.MatchString(s) }

// SafeMachineText checks bounded UTF-8 metadata without terminal controls.
func SafeMachineText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// ValidateMachine checks optional local registry metadata, preserving old configs.
func (c Config) ValidateMachine() error {
	if c.MachineName != "" && !ValidMachineName(c.MachineName) {
		return errors.New("machine_name must contain 1 to 40 lowercase letters, digits, or hyphens, starting with a letter or digit")
	}
	a := c.MachineAssignment
	if a == nil {
		return nil
	}
	if len(a.DestinationID) != 64 || strings.Trim(a.DestinationID, "0123456789abcdef") != "" {
		return errors.New("invalid machine assignment destination")
	}
	switch a.Kind {
	case "aws_profile", "r2_shared", "r2_own", "r2_unknown":
	default:
		return errors.New("unsupported machine assignment kind")
	}
	if !SafeMachineText(a.AccessKeyID, 128) {
		return errors.New("invalid machine assignment access key ID")
	}
	for _, id := range []string{a.RecipientID, a.IssuerID, a.SlotID, a.SharedWith, a.PairingID, a.PairedFrom} {
		if id != "" && !ValidMachineID(id) {
			return errors.New("invalid machine assignment identifier")
		}
	}
	if a.Kind == "r2_own" && (a.AccessKeyID == "" || a.RecipientID == "" || a.IssuerID == "") {
		return errors.New("own key assignment needs access key, recipient, and issuer identifiers")
	}
	if a.Kind == "r2_shared" && (a.AccessKeyID == "" || a.SharedWith == "") {
		return errors.New("shared key assignment needs access key and sharing identifiers")
	}
	if a.PairedAt != nil && a.PairedAt.IsZero() {
		return errors.New("invalid machine assignment pairing time")
	}
	return nil
}
