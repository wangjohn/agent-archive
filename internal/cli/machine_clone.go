package cli

import (
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// hostFingerprint is this machine's fingerprint for the copied-data check:
// the Linux machine ID's digest, and "" on any other system (macOS, whose
// copied data directory the Migration Assistant section of the multiple-Macs
// guide covers, records nothing) or when the system has no machine ID to
// read.
func (e Env) hostFingerprint() string {
	if e.operatingSystem() != platform.Linux {
		return ""
	}
	if e.HostFingerprint != nil {
		return e.HostFingerprint()
	}
	return local.HostFingerprint()
}

// copiedFromAnotherMachine reports whether cfg's data directory was set up on
// a different machine than this one: it recorded a host ID, this machine has
// one, and they differ. Cloning a VM or a container image copies the data
// directory, machine ID and all, so two live machines would claim the same
// sessions and publish over each other (the Linux counterpart of copying the
// directory with the Migration Assistant or a Time Machine restore).
//
// The signal is the machine ID the clone's operating system ended up with, so
// it sees a clone whose ID was regenerated (what cloud images and sysprep
// do) and cannot see one that kept it (a disk copied as it is); and a machine
// whose operating system was reinstalled over the same home looks like
// another. It says nothing when either side has no ID.
func (e Env) copiedFromAnotherMachine(cfg config.Config) bool {
	if cfg.HostID == "" {
		return false
	}
	current := e.hostFingerprint()
	return current != "" && current != cfg.HostID
}

// copiedMachineWarning is what to tell someone whose data directory was set up
// on another machine: the first line, then the lines that follow it.
func copiedMachineWarning(home string) (string, []string) {
	return "This data directory was set up on a different machine: the host ID it recorded is not this machine's.",
		[]string{
			"If it was copied here (a cloned VM or container image, a restored backup) and the original is still",
			"in use, the two claim the same sessions and publish over each other. On the copy, run",
			"agent-archive uninstall --delete-local-data, then agent-archive setup, which gives it a new machine ID.",
			"If the original is retired, or this is the same machine with its operating system reinstalled,",
			"remove the \"host_id\" entry from " + filepath.Join(home, "config.json") + " and this stops.",
		}
}
