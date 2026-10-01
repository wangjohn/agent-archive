package local

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// machineIDPaths are where a Linux system keeps its machine ID, in the order
// to try: systemd's, then D-Bus's (a link to the first on most systems, and a
// file of its own on one without systemd).
var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// HostFingerprint identifies the machine the program runs on, for telling
// that a data directory was made on another one: a digest of the Linux
// machine ID (/etc/machine-id), or "" when there is none to read or it is
// not a machine ID (the file is empty on an image until first boot, holds
// "uninitialized" on a read-only system that has not assigned one yet, and is
// absent in most containers).
//
// The machine ID is confidential by systemd's own rules (it identifies the
// host to anything that can read it), so what is recorded is a digest of it
// that names this program, not the ID, which is what systemd recommends
// applications do.
//
// A system whose /etc is read-only and has no ID of its own gets a transient
// one at every boot, which systemd mounts over /etc/machine-id (machine-id(5));
// it would look like another machine after each reboot, so a machine ID that
// is mounted there gives "" too (D-Bus's is a link to it on such a system).
//
// It is a signal, not proof: a clone that kept its machine ID looks like the
// machine it was cloned from, and a machine whose ID was regenerated looks
// like another. Call it only on Linux.
func HostFingerprint() string { return hostFingerprint(os.ReadFile) }

func hostFingerprint(read func(string) ([]byte, error)) string {
	if mountedOver(read, machineIDPaths[0]) {
		return ""
	}
	for _, path := range machineIDPaths {
		data, err := read(path)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); validMachineID(id) {
			sum := sha256.Sum256([]byte("agent-archive host fingerprint v1\x00" + id))
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
}

// mountedOver reports whether something is mounted at path, as this process's
// mount table (/proc/self/mountinfo, whose fifth field is the mount point)
// says. A table that cannot be read says nothing is.
func mountedOver(read func(string) ([]byte, error), path string) bool {
	data, err := read("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == path {
			return true
		}
	}
	return false
}

// validMachineID reports whether id is what machine-id(5) describes: 32
// lowercase hexadecimal digits.
func validMachineID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
