package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// loadLaunchAgent bootstraps the plist into this user's GUI session and
// reports launchctl's own output when that fails. launchctl is stubbed: no
// test here reaches launchd.
func TestLoadLaunchAgentBootstrapsThePlist(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "com.agent-archive.collector.abc.plist")
	var calls [][]string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	})
	if err := loadLaunchAgent(plist); err != nil {
		t.Fatal(err)
	}
	want := []string{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("launchctl calls %q, want %q", calls, want)
	}

	stubLaunchctl(t, func(...string) ([]byte, error) {
		return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
	})
	err := loadLaunchAgent(plist)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err %v, want launchctl's output", err)
	}
}

// unloadLaunchAgent boots out a job only once launchctl print shows it was
// loaded from this very plist, by its service target; a job loaded from any
// other plist, or one launchctl cannot describe, is left alone and reported.
func TestUnloadLaunchAgentBootsOutOnlyItsOwnJob(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "com.agent-archive.collector.abc.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "com.agent-archive.collector.abc.plist")
	target := fmt.Sprintf("gui/%d/com.agent-archive.collector.abc", os.Getuid())
	for _, tc := range []struct {
		name      string
		print     string
		printErr  error
		bootErr   error
		bootedOut bool
		wantErr   string
	}{
		{name: "running from this plist", print: "path = " + plist + "\nstate = running\n", bootedOut: true},
		{name: "loaded from this plist", print: "path = " + plist + "\nstate = waiting\n", bootedOut: true},
		{name: "not loaded", print: "Could not find service", printErr: errors.New("exit status 113")},
		{name: "loaded from another plist", print: "path = " + other + "\nstate = running\n", wantErr: "belongs to another installation"},
		{name: "print names no plist", print: "state = running\n", wantErr: "cannot confirm"},
		{name: "print fails", print: "some failure", printErr: errors.New("exit status 1"), wantErr: "cannot confirm"},
		{name: "bootout fails", print: "path = " + plist + "\nstate = running\n", bootErr: errors.New("exit status 5"), bootedOut: true, wantErr: "launchctl bootout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			stubLaunchctl(t, func(args ...string) ([]byte, error) {
				calls = append(calls, args)
				if args[0] == "print" {
					return []byte(tc.print), tc.printErr
				}
				return []byte("bootout output"), tc.bootErr
			})
			err := unloadLaunchAgent(plist)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if !slices.Equal(calls[0], []string{"print", target}) {
				t.Fatalf("first call %q", calls[0])
			}
			bootedOut := len(calls) == 2 && slices.Equal(calls[1], []string{"bootout", target})
			if bootedOut != tc.bootedOut || len(calls) > 2 {
				t.Fatalf("launchctl calls %q, booted out %v, want %v", calls, bootedOut, tc.bootedOut)
			}
		})
	}
}

// A label is not proof of ownership: launchd reports the plist it loaded a
// job from, and a job loaded from any other file is another installation's.
func TestParseJobStateComparesTheLoadedPlist(t *testing.T) {
	ours := filepath.Join(t.TempDir(), "com.agent-archive.collector.plist")
	for _, tc := range []struct {
		output string
		err    error
		want   string
	}{
		{"gui/501/com.agent-archive.collector = {\n\tpath = " + ours + "\n\tstate = running\n}", nil, "running"},
		{"\tstate = waiting\n\tpath = " + ours + "\n", nil, "loaded"},
		{"\tpath = /Users/someone/Library/LaunchAgents/com.agent-archive.collector.plist\n\tstate = running\n", nil, jobAnotherInstallation},
		{"\tstate = running\n", nil, "unknown"},
		{"Could not find service \"x\" in domain for user gui: 501", fmt.Errorf("exit status 113"), "missing"},
		{"boom", fmt.Errorf("exit status 1"), "unknown"},
	} {
		if got := parseJobState(tc.output, tc.err, ours); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.output, got, tc.want)
		}
	}
}
