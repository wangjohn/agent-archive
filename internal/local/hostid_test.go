package local

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

const (
	machineA = "0123456789abcdef0123456789abcdef"
	machineB = "fedcba9876543210fedcba9876543210"
)

func readers(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, fs.ErrNotExist
	}
}

// The fingerprint is stable for a machine, differs between machines, and is
// not the machine ID itself.
func TestHostFingerprintIsADigestOfTheMachineID(t *testing.T) {
	t.Parallel()
	a := hostFingerprint(readers(map[string]string{"/etc/machine-id": machineA + "\n"}))
	again := hostFingerprint(readers(map[string]string{"/etc/machine-id": machineA}))
	b := hostFingerprint(readers(map[string]string{"/etc/machine-id": machineB + "\n"}))
	if a == "" || a != again {
		t.Errorf("fingerprint %q, %q: want one stable digest whatever the trailing newline", a, again)
	}
	if a == b {
		t.Error("two machines have one fingerprint")
	}
	if strings.Contains(a, machineA) || len(a) != 64 {
		t.Errorf("fingerprint %q is not a digest of the ID", a)
	}
}

// D-Bus's copy is used only when systemd's is unusable, and what is not a
// machine ID is no fingerprint.
func TestHostFingerprintFallsBackAndRefusesWhatIsNotAMachineID(t *testing.T) {
	t.Parallel()
	dbus := map[string]string{"/var/lib/dbus/machine-id": machineA}
	want := hostFingerprint(readers(map[string]string{"/etc/machine-id": machineA}))
	if got := hostFingerprint(readers(dbus)); got != want {
		t.Errorf("D-Bus's machine ID gives %q, want the same as systemd's %q", got, want)
	}
	for name, files := range map[string]map[string]string{
		"no file":                     {},
		"empty until first boot":      {"/etc/machine-id": ""},
		"newline only":                {"/etc/machine-id": "\n"},
		"uninitialized":               {"/etc/machine-id": "uninitialized\n"},
		"too short":                   {"/etc/machine-id": machineA[:31]},
		"too long":                    {"/etc/machine-id": machineA + "0"},
		"upper case":                  {"/etc/machine-id": strings.ToUpper(machineA)},
		"not hexadecimal":             {"/etc/machine-id": strings.Repeat("g", 32)},
		"a second ID after the first": {"/etc/machine-id": machineA + "\n" + machineB},
	} {
		if got := hostFingerprint(readers(files)); got != "" {
			t.Errorf("%s: fingerprint %q, want none", name, got)
		}
	}
	if got := hostFingerprint(readers(map[string]string{"/etc/machine-id": "uninitialized", "/var/lib/dbus/machine-id": machineA})); got != want {
		t.Errorf("an unusable systemd ID should fall back to D-Bus's: %q, want %q", got, want)
	}
	// A transient ID, mounted over the file at boot on a read-only /etc, is
	// another at every boot: it is no fingerprint, and neither is D-Bus's,
	// which is a link to it there. Another mount is not that.
	transient := "22 1 0:21 / /proc rw,nosuid shared:5 - proc proc rw\n" +
		"35 26 0:30 /machine-id /etc/machine-id ro,relatime shared:12 - tmpfs tmpfs rw\n"
	if got := hostFingerprint(readers(map[string]string{"/proc/self/mountinfo": transient, "/etc/machine-id": machineA, "/var/lib/dbus/machine-id": machineA})); got != "" {
		t.Errorf("a transient machine ID gives fingerprint %q, want none", got)
	}
	other := "22 1 0:21 / /proc rw,nosuid shared:5 - proc proc rw\n35 26 0:30 / /etc/machine-id.d rw - tmpfs tmpfs rw\n"
	if got := hostFingerprint(readers(map[string]string{"/proc/self/mountinfo": other, "/etc/machine-id": machineA})); got != want {
		t.Errorf("an unrelated mount changes the fingerprint to %q, want %q", got, want)
	}
	if got := hostFingerprint(func(string) ([]byte, error) { return nil, errors.New("permission denied") }); got != "" {
		t.Errorf("unreadable files give %q, want none", got)
	}
}
