package config

import "errors"

// SetCodexCaptureScope records an explicit choice; reconciliation commits its boundary.
func SetCodexCaptureScope(c *Config, scope CodexCaptureScope) error {
	if scope != CodexIncludedProjects && scope != CodexAllProjects {
		return errors.New("invalid Codex capture scope")
	}
	if c.CodexCapture == nil {
		c.CodexCapture = &CodexCaptureConfig{}
	} else {
		scopeCopy := *c.CodexCapture
		c.CodexCapture = &scopeCopy
	}
	c.CodexCapture.Scope = scope
	return prepareDiscoveryConfig(c)
}
