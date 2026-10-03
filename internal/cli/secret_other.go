//go:build !darwin && !linux

package cli

import "golang.org/x/term"

// readSecret reads a line from the terminal fd with echo off. Only macOS
// and Linux read it themselves (secret_unix.go).
func readSecret(fd int) ([]byte, error) { return term.ReadPassword(fd) }
