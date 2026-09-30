// Package platform is the one place that knows which operating system the
// program runs on, and where that system keeps the things agent-archive looks
// for. Every other package takes a platform.OS (or a Locations built from
// one) as an argument, so a test answers for macOS or Linux on any host, and
// none of them reads runtime.GOOS (TestOnlyPlatformReadsRuntimeGOOS).
//
// The package is pure: it runs no program, opens no file and reads no
// environment or clock of its own. What needs the system (the per-user
// temporary directory macOS reports, resolving symlinks) comes in through
// LocationDeps, whose real implementations live with the code that already
// had them. TestPlatformImportBoundary keeps it that way, and depguard says
// the same on macOS.
package platform

import "runtime"

// OS is an operating system agent-archive knows how to answer for. Anything
// else is Unknown, and a caller that branches on an OS must decide what
// Unknown means for it: never quietly the Linux answer (Linux is one of the
// two systems this program is built and tested for, not the default for the
// rest). Each caller's choice is written where it branches.
type OS string

const (
	// Darwin is macOS.
	Darwin OS = "darwin"
	// Linux is Linux.
	Linux OS = "linux"
	// Unknown is any other system. The zero value of OS is not Unknown, so
	// that a struct field a caller leaves unset can mean "the real system"
	// (Current) where its comment says so; OS("") and any other string that
	// is not Darwin or Linux is handled as Unknown by every function here.
	Unknown OS = "unknown"
)

// Current is the operating system this process runs on. It is the only
// non-test reader of runtime.GOOS in the repository (build-tagged files
// choose their code at compile time instead).
func Current() OS { return fromGOOS(runtime.GOOS) }

// fromGOOS names the runtime.GOOS value goos: Darwin, Linux, or Unknown.
func fromGOOS(goos string) OS {
	switch system := OS(goos); system {
	case Darwin, Linux:
		return system
	case Unknown:
	}
	return Unknown
}
