//go:build !darwin && !linux

package cli

import "io"

// terminalKeys is Env.openKeyTerminal's default (openTerminalKeys). Only
// macOS and Linux read keys one at a time; elsewhere the browser reads
// lines.
func terminalKeys(io.Reader) (keyTerminal, bool) { return nil, false }
