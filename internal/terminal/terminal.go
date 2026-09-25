// Package terminal writes messages meant for a person at a terminal.
//
// A failed write to stdout or stderr has no one left to report to, so these
// helpers discard write errors on purpose. Use them only for terminal output:
// anything written to a file, a pipe another program reads, or a network
// connection must check its errors instead.
package terminal

import (
	"fmt"
	"io"
)

// Printf formats a message to w, as fmt.Fprintf does, ignoring write errors.
func Printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// Println writes its operands and a newline to w, as fmt.Fprintln does,
// ignoring write errors.
func Println(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// Print writes its operands to w, as fmt.Fprint does, ignoring write errors.
func Print(w io.Writer, args ...any) {
	_, _ = fmt.Fprint(w, args...)
}
