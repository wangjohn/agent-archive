package setupjournal

// Launchd is the part of launchd a setup transaction drives: the state of
// the job a plist defines, and loading or stopping it. internal/cli provides
// it from its Env, so its tests' stand-ins apply here too; this package's
// tests pass a fake. Nothing in this package runs launchctl itself.
type Launchd interface {
	// JobState reports "loaded", "running", "missing", "unknown", or
	// JobAnotherInstallation for the job plist defines, without changing it.
	JobState(plist string) string
	// Load loads (bootstraps) the job plist defines.
	Load(plist string) error
	// Unload stops (boots out) the job plist defines.
	Unload(plist string) error
}

// JobActive reports whether a launchd job state from Launchd.JobState means
// the job is loaded, whether or not it is running at the moment.
func JobActive(job string) bool {
	//lint:ignore LV1001 Launchd.JobState reports launchd states as plain strings, and tests stub it with string-returning funcs
	return job == "loaded" || job == "running"
}

// JobAnotherInstallation is the job state of a label launchd has loaded
// from a plist other than the one asked about: the job belongs to another
// installation (the user's real one, seen from a sandboxed HOME, say), and
// nothing here may stop or replace it.
const JobAnotherInstallation = "another_installation"
