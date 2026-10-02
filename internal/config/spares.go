package config

import (
	"errors"
	"strings"
)

// SpareTarget returns the desired unused key count, including legacy defaults.
func (c Config) SpareTarget() int {
	if c.SpareKeys == nil {
		return 2
	}
	return *c.SpareKeys
}

// ValidateSpares checks the advisory spare settings, never granting eligibility.
func (c Config) ValidateSpares() error {
	if c.SpareTarget() < 0 || c.SpareTarget() > 5 || len(c.SpareCredentialRefs) > 5 {
		return errors.New("spare_keys must be 0 through 5")
	}
	seen := map[string]bool{}
	for _, ref := range c.SpareCredentialRefs {
		if !strings.HasPrefix(ref, "issued-") || !ValidMachineID(strings.TrimPrefix(ref, "issued-")) || seen[ref] {
			return errors.New("invalid spare credential index")
		}
		seen[ref] = true
	}
	return nil
}
