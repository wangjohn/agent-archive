package platform

import (
	"runtime"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// Current names the system the tests run on. The expected value is spelled
// out per runtime.GOOS here, in a test, on purpose: it is the check that
// Current does not answer for a different system.
func TestCurrentIsTheRunningSystem(t *testing.T) {
	t.Parallel()
	want := Unknown
	switch runtime.GOOS {
	case "darwin":
		want = Darwin
	case "linux":
		want = Linux
	}
	if got := Current(); got != want {
		t.Fatalf("Current() = %q on %s, want %q", got, runtime.GOOS, want)
	}
}

// Only darwin and linux are known; every other GOOS, and the empty string,
// is Unknown and never Linux.
func TestFromGOOS(t *testing.T) {
	t.Parallel()
	for goos, want := range map[string]OS{
		"darwin":  Darwin,
		"linux":   Linux,
		"freebsd": Unknown,
		"windows": Unknown,
		"":        Unknown,
		"Darwin":  Unknown,
		"Linux":   Unknown,
		"unknown": Unknown,
	} {
		if got := fromGOOS(goos); got != want {
			t.Errorf("fromGOOS(%q) = %q, want %q", goos, got, want)
		}
	}
}
