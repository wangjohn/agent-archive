//go:build !locktrace

package local

// Without the locktrace build tag, lock holds are not traced (see
// locktrace.go).

func traceSlow(string) {}

func traceHold(_ string, release func()) func() { return release }
