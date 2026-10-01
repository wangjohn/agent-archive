package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// allowNetworkHomeFlag is setup's opt-in for a home on a network filesystem.
const allowNetworkHomeFlag = "--allow-network-home"

// readMountTable reads this process's mount table for the check below; the
// package's tests replace it, so none reads the machine running them.
var readMountTable = local.ReadMountTable

// mountTable is the mount table the network-filesystem check reads: the
// Env's own, or this process's.
func (e Env) mountTable() ([]byte, error) {
	if e.MountTable != nil {
		return e.MountTable()
	}
	return readMountTable()
}

// networkSpot is a directory of the installation that is on a network
// filesystem.
type networkSpot struct {
	// unit is whether this is the systemd unit directory; else it is the data
	// directory.
	unit  bool
	place local.Placement
}

// where says what filesystem the directory is on, as a clause of a sentence.
func (s networkSpot) where() string {
	return fmt.Sprintf("on a network filesystem (%s, mounted at %s)", s.place.Type, s.place.Mount)
}

// networkSpots are the data directory and, on Linux, the systemd unit
// directory (under the user's home, which AGENT_ARCHIVE_HOME does not move)
// of the installation, in that order, that are on a network filesystem
// (local.NetworkFilesystem). Both are looked up through the nearest directory
// above them that exists, since setup has not made them yet. It is Linux
// only: on any other system it is empty and reads nothing, so macOS's setup,
// refresh and status are as they were. A mount table that cannot be read, or
// a directory that cannot be placed in it, gives no spot: an answer that
// cannot be told never stops anything.
func (e Env) networkSpots(home, userHome string) []networkSpot {
	if e.operatingSystem() != platform.Linux {
		return nil
	}
	probe := local.FilesystemProbe{MountTable: e.mountTable}
	var spots []networkSpot
	for _, dir := range []struct {
		unit bool
		path string
	}{
		{false, home},
		{true, platform.NewLocations(platform.Linux, userHome, e.getenv, platform.LocationDeps{}).UserUnitDir},
	} {
		if dir.path == "" {
			continue
		}
		if place, ok := probe.Where(dir.path); ok && place.Network() {
			spots = append(spots, networkSpot{unit: dir.unit, place: place})
		}
	}
	return spots
}

// networkHomeAllowed is whether the person allowed a network home: this
// run's --allow-network-home, or the allowance the saved configuration in
// home records (setup records it once it was needed). A configuration that
// cannot be read allows nothing here; the commands report it themselves.
func networkHomeAllowed(flag bool, home string) bool {
	if flag {
		return true
	}
	saved, found, err := config.Load(home)
	return err == nil && found && saved.AllowNetworkHome
}

// networkHomeOptIn is Config.AllowNetworkHome after a setup run: the opt-in
// (this run's flag, or the saved configuration's) while a directory of the
// installation is on a network filesystem. It is not recorded where it is not
// needed, so a home that moves to local disk drops it, and one that later
// moves to a network filesystem is refused again.
func (e Env) networkHomeOptIn(home, userHome string, flag bool, saved config.Config) bool {
	return (flag || saved.AllowNetworkHome) && len(e.networkSpots(home, userHome)) > 0
}

// harm is why a shared home is a problem, for this directory.
func (s networkSpot) harm() string {
	if s.unit {
		return "Every machine that mounts this home directory loads the units in it, so each of them would run the collector."
	}
	return "Machines that share this directory would share one machine ID and claim the same sessions; file locks are not reliable over a network filesystem, so concurrent collectors can corrupt its state; and every machine's background job would run against the same files."
}

// networkHomeChecks are setup's checks of where its files go, which run
// before anything is created or locked (the lock is a file lock in the data
// directory). Each directory on a network filesystem is a failed check naming
// it, the filesystem, why it matters and what to do, unless allowed (the
// opt-in), when it passes, still saying so. Nothing is returned off Linux or
// when neither directory is on a network filesystem.
func (e Env) networkHomeChecks(home, userHome string, allowed bool) preflightChecks {
	var checks preflightChecks
	for _, spot := range e.networkSpots(home, userHome) {
		label, path := "Data directory", displayPath(spot.place.Path, userHome)
		if spot.unit {
			label = "Systemd unit directory"
		}
		check := preflightCheck{Label: label, Detail: path + " is " + spot.where()}
		if allowed {
			check.Detail += ", which you allowed with " + allowNetworkHomeFlag + "; it must stay on one machine"
			check.OK = true
			checks = append(checks, check)
			continue
		}
		check.Problem = spot.harm()
		if spot.unit {
			check.Fix = "AGENT_ARCHIVE_HOME does not move the unit directory: it is under your home directory, where systemd looks for user units. If only one machine ever mounts this home directory, run agent-archive setup " + allowNetworkHomeFlag + "."
		} else {
			check.Fix = "Keep the data directory on local disk: set AGENT_ARCHIVE_HOME to a path there and run agent-archive setup again. If only one machine ever mounts this directory, run agent-archive setup " + allowNetworkHomeFlag + " instead."
		}
		checks = append(checks, check)
	}
	return checks
}

// refuseNetworkHome is setup's check of a network home, before it creates or
// locks anything. It prints what it found, and when a directory is on a
// network filesystem that was not allowed, the refusal as setup's other
// refusals before its first question are, and reports the exit code with
// refused true.
func refuseNetworkHome(opts setupOptions, stdout, stderr io.Writer, env Env) (code int, refused bool) {
	// readHome, not home, which makes the data directory: the one place a
	// refusal must not leave it.
	home, err := env.readHome()
	if err != nil {
		return 0, false
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return 0, false
	}
	checks := env.networkHomeChecks(home, userHome, networkHomeAllowed(opts.allowNetworkHome, home))
	if len(checks) == 0 {
		return 0, false
	}
	checks.write(stdout, styleFor(stdout))
	if !checks.blocked() {
		return 0, false
	}
	blocker := &preflightError{checks: checks, yes: opts.yes}
	if opts.yes {
		// On standard error too, which is what a script reads.
		terminal.Printf(stderr, "Setup incomplete: %v\n", blocker)
		terminal.Println(stderr, blocker.guidance())
		return 1, true
	}
	terminal.Println(stderr, "Setup incomplete. "+blocker.guidance())
	return 1, true
}

// networkRefreshProblem is why setup --refresh must not run on a home on a
// network filesystem that was never allowed, or "" when it may.
func (e Env) networkRefreshProblem(cfg config.Config, home, userHome string) string {
	if cfg.AllowNetworkHome {
		return ""
	}
	spots := e.networkSpots(home, userHome)
	if len(spots) == 0 {
		return ""
	}
	var found []string
	for _, spot := range spots {
		name := "the data directory"
		if spot.unit {
			name = "the systemd unit directory"
		}
		found = append(found, fmt.Sprintf("%s %s is %s", name, displayPath(spot.place.Path, userHome), spot.where()))
	}
	return strings.Join(found, ", and ") + ", which this installation was not set up to allow. " + spots[0].harm() + " If only one machine ever mounts this home, run agent-archive setup " + allowNetworkHomeFlag
}

// networkHomeWarnings are status's warnings for a data directory or unit
// directory on a network filesystem, whether or not it was allowed: that it
// is safe only while one machine uses it stays in view.
func (e Env) networkHomeWarnings(cfg config.Config, home, userHome string) []string {
	var warnings []string
	for _, spot := range e.networkSpots(home, userHome) {
		name := "The data directory"
		if spot.unit {
			name = "The systemd unit directory"
		}
		text := fmt.Sprintf("%s %s is %s. It is safe only while one machine uses this home. %s", name, spot.place.Path, spot.where(), spot.harm())
		if cfg.AllowNetworkHome {
			text += " You allowed this with agent-archive setup " + allowNetworkHomeFlag + "."
		} else {
			text += " Setup now refuses a network filesystem. If only one machine ever mounts it, run agent-archive setup " + allowNetworkHomeFlag + " to allow it"
			if !spot.unit {
				text += "; otherwise move the data directory to local disk with AGENT_ARCHIVE_HOME"
			}
			text += "."
		}
		warnings = append(warnings, text)
	}
	return warnings
}
