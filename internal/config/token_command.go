package config

import (
	"errors"
	"strings"
)

// ValidateCloudflareTokenCommand checks an optional argv command, never shell syntax.
func (c Config) ValidateCloudflareTokenCommand() error {
	args := c.CloudflareTokenCommand
	if len(args) > 64 {
		return errors.New("cloudflare_token_command has too many arguments")
	}
	for _, arg := range args {
		if len(arg) > 2048 || !SafeMachineText(arg, 2048) || strings.ContainsRune(arg, 0) {
			return errors.New("invalid cloudflare_token_command argument")
		}
	}
	if len(args) > 0 && strings.TrimSpace(args[0]) == "" {
		return errors.New("cloudflare_token_command needs a program")
	}
	return nil
}
