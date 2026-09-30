package systemd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// minVersion is the oldest systemd that logs with StandardOutput=append:,
// which the service unit relies on. RHEL 8's systemd is 239, which has it
// from the package release rhelAppendRelease on (see appendBackported).
const minVersion = 240

// rhelAppendRelease is the first release of RHEL 8's systemd 239 package
// (239-32, RHEL 8.3) with StandardOutput=append: backported (Red Hat bug
// 1809175, "Add support for opening files for appending" in the package's
// changelog). Rocky, Alma, Oracle Linux and CentOS Stream 8 rebuild the same
// package; checked against Rocky 8's 239-82.el8_10, which appends.
const rhelAppendRelease = 32

// The fixes Problems offer. The user bus is the common failure on Linux:
// an SSH session or a container has no user manager to talk to.
const (
	busFix     = "Run this from a login session that has a systemd user bus (a desktop or console login, or ssh with pam_systemd), or run `loginctl enable-linger` once so the user manager runs without a login, then check that `systemctl --user status` works"
	genericFix = "Check that `systemctl --user status` works in Terminal"
	oldFix     = "Upgrade systemd to version 240 or newer (on RHEL 8 or a rebuild of it, update to 8.3 or later): the collector's logs need StandardOutput=append:"
)

// loadState is a unit's LoadState: whether systemd could read its definition.
type loadState string

// The LoadStates the adapter tells apart; systemd has others (bad-setting,
// error, merged), which all mean it could not load the unit as it is.
const (
	loadLoaded   loadState = "loaded"
	loadNotFound loadState = "not-found"
	loadStub     loadState = "stub"
	loadMasked   loadState = "masked"
)

// activeState is a unit's ActiveState.
type activeState string

// The ActiveStates the adapter tells apart.
const (
	stateActive       activeState = "active"
	stateActivating   activeState = "activating"
	stateReloading    activeState = "reloading"
	stateRefreshing   activeState = "refreshing"
	stateDeactivating activeState = "deactivating"
)

// unit is what `systemctl show` says about one unit.
type unit struct {
	id       string
	load     loadState
	active   activeState
	fragment string
	dropIns  []string
}

// exists is whether systemd has the unit loaded from a file.
func (u unit) exists() bool { return u.load == loadLoaded }

// alive is whether the unit is starting, running or reloading.
func (u unit) alive() bool {
	switch u.active {
	case stateActive, stateActivating, stateReloading, stateRefreshing:
		return true
	case stateDeactivating:
	}
	return false
}

// busy is whether the unit is alive or stopping.
func (u unit) busy() bool { return u.alive() || u.active == stateDeactivating }

// parseShow reads `systemctl show` for several units: blocks of Key=Value
// lines, one block per unit, separated by an empty line, each naming its unit
// in Id, its properties in no fixed order. A property named with --property
// is printed even when empty (FragmentPath= for a unit not found, on 239 to
// 255); an unfiltered `show` leaves empty ones out, so a missing one is empty
// too, and a property this reader does not use is skipped.
func parseShow(output string) map[string]unit {
	units := map[string]unit{}
	var current unit
	flush := func() {
		if current.id != "" {
			units[current.id] = current
		}
		current = unit{}
	}
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if !ok {
			flush()
			continue
		}
		//lint:ignore LV1001 property names come from systemctl's output; any other property is skipped
		switch key {
		case "Id":
			current.id = value
		case "LoadState":
			current.load = loadState(value)
		case "ActiveState":
			current.active = activeState(value)
		case "FragmentPath":
			current.fragment = value
		case "DropInPaths":
			current.dropIns = strings.Fields(value)
		}
	}
	flush()
	return units
}

// probed is what asking systemd about a job comes to.
type probed struct {
	state       scheduler.JobState
	problem     *scheduler.Problem
	timer       unit
	service     unit
	degraded    []string
	timerFile   string
	serviceFile string
}

// probe asks about the job ref names: the version of systemd, then the state
// of its timer and service units. It says what is wrong when the job cannot be
// acted on: a Problem for one systemd runs from other unit files, one it
// cannot describe, and one it is too old to run.
func (s Scheduler) probe(ctx context.Context, site scheduler.Site, ref scheduler.Ref) probed {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	p := probed{state: scheduler.Unknown, timerFile: s.timerPath(site, ref), serviceFile: s.servicePath(site, ref)}
	cannotTell := func(fix string) probed {
		p.state, p.problem = scheduler.Unknown, &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Expected: p.timerFile, Fix: fix}
		return p
	}
	output, err := s.Run(ctx, "systemctl", "--version")
	if err != nil {
		return cannotTell(unreachableFix(string(output) + " " + err.Error()))
	}
	if version, ok := parseVersion(string(output)); ok && version < minVersion && !appendBackported(string(output)) {
		return cannotTell(fmt.Sprintf("%s (this is systemd %d)", oldFix, version))
	}
	timerName, serviceName := string(ref)+".timer", string(ref)+".service"
	output, err = s.Run(ctx, "systemctl", "--user", "show", timerName, serviceName, "--property=Id,LoadState,ActiveState,SubState,FragmentPath,DropInPaths")
	if err != nil {
		return cannotTell(unreachableFix(string(output) + " " + err.Error()))
	}
	units := parseShow(string(output))
	var found bool
	if p.timer, found = units[timerName]; found {
		p.service, found = units[serviceName]
	}
	if !found {
		return cannotTell(genericFix)
	}
	return s.judge(p, ref)
}

// judge reads the two units into a state, in this order: a unit systemd cannot
// load as it is (masked, or a setting it rejects), one loaded from no file,
// and one still running after its file was deleted and the manager reloaded
// (not-found and active) are unknown; a unit loaded from a file that is not this site's is another
// installation's; a running service is running, whatever the timer is doing (a
// stop must reach it); a timer that is active is loaded, whatever the last
// run's result (a failed service does not stop the timer); and anything else,
// not-found and inactive included, is missing.
func (s Scheduler) judge(p probed, ref scheduler.Ref) probed {
	names := [2]string{string(ref) + ".timer", string(ref) + ".service"}
	units, ours := [2]unit{p.timer, p.service}, [2]string{p.timerFile, p.serviceFile}
	for i, u := range units {
		var fix string
		switch u.load {
		case loadLoaded, loadNotFound, loadStub, "":
			switch {
			case u.exists() && u.fragment == "":
				fix = genericFix + "; systemd runs " + names[i] + " from no unit file"
			case !u.exists() && u.busy():
				// Its unit file was deleted and the manager reloaded while it
				// ran: nothing says whose it was, and it runs on.
				fix = "Run `systemctl --user stop " + names[0] + " " + names[1] + "`: systemd still runs " + names[i] + ", whose unit file is gone"
			}
		case loadMasked:
			fix = "Run `systemctl --user unmask " + names[0] + " " + names[1] + "`"
		default:
			fix = "Run `systemctl --user status " + names[i] + "` to see why systemd cannot load it (" + string(u.load) + ")"
		}
		if fix != "" {
			p.state, p.problem = scheduler.Unknown, &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Expected: ours[i], Fix: fix}
			return p
		}
	}
	for i, u := range units {
		if u.exists() && !local.SameLocation(u.fragment, ours[i]) {
			p.state = scheduler.AnotherInstallation
			p.problem = &scheduler.Problem{Kind: scheduler.ProblemNotOwned, Ref: ref, LoadedFrom: u.fragment, Expected: ours[i], Fix: "Uninstall that installation first, or set AGENT_ARCHIVE_HOME to a directory of this installation's own"}
			return p
		}
	}
	switch {
	case p.service.busy():
		p.state = scheduler.Running
	case p.timer.alive():
		p.state = scheduler.Loaded
	default:
		p.state = scheduler.Missing
	}
	if p.state.Active() {
		for _, u := range units {
			if own := ownDropIns(u); len(own) > 0 {
				p.degraded = append(p.degraded, fmt.Sprintf("a drop-in overrides %s (%s), so its settings differ from the unit file's", u.id, strings.Join(own, ", ")))
			}
		}
	}
	return p
}

// ownDropIns are the drop-ins that name u: those in a directory other than
// the one for every unit of its type (service.d, timer.d), which a
// distribution may ship (Fedora's user service.d/10-timeout-abort.conf) and
// which says nothing about this job.
func ownDropIns(u unit) []string {
	var own []string
	typeWide := strings.TrimPrefix(filepath.Ext(u.id), ".") + ".d"
	for _, path := range u.dropIns {
		if filepath.Base(filepath.Dir(path)) != typeWide {
			own = append(own, path)
		}
	}
	return own
}

// unreachableFix is the next step for a systemctl that failed, from what it
// printed: no user bus (an SSH session, a container without one), no systemd
// as the init system, or no systemctl at all.
func unreachableFix(output string) string {
	switch {
	case strings.Contains(output, "Failed to connect to"), strings.Contains(output, "not been booted"), strings.Contains(output, "Can't operate"):
		return busFix
	case strings.Contains(output, "not found"):
		return "Install systemd: systemctl was not found on PATH"
	}
	return genericFix
}

// parseVersion reads the version from the first line of `systemctl
// --version`: "systemd 252 (252.22-1~deb12u1)".
func parseVersion(output string) (int, bool) {
	fields := strings.Fields(strings.SplitN(output, "\n", 2)[0])
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0, false
	}
	version, err := strconv.Atoi(fields[1])
	return version, err == nil
}

// appendBackported reports whether `systemctl --version` names RHEL 8's
// systemd 239 at a package release with StandardOutput=append: backported:
// "systemd 239 (239-82.el8_10.19)", a release of rhelAppendRelease or later
// whose dist tag is el8, wherever it comes (Oracle Linux puts a number of its
// own before it: "239-82.0.13.el8_10.19"). Anything else is not.
func appendBackported(output string) bool {
	fields := strings.Fields(strings.SplitN(output, "\n", 2)[0])
	if len(fields) < 3 || fields[0] != "systemd" || fields[1] != "239" {
		return false
	}
	release, prefixed := strings.CutPrefix(fields[2], "(239-")
	release, suffixed := strings.CutSuffix(release, ")")
	if !prefixed || !suffixed {
		return false
	}
	parts := strings.Split(release, ".")
	if n, err := strconv.Atoi(parts[0]); err != nil || strings.Trim(parts[0], "0123456789") != "" || n < rhelAppendRelease {
		return false
	}
	return slices.ContainsFunc(parts[1:], func(part string) bool { return part == "el8" || strings.HasPrefix(part, "el8_") })
}

// Inspect asks systemd about the job ref names, reads its answer against the
// unit files at site (see the state map in the port's design and judge), and
// reads the unit files themselves (Definition). A job that works while the user
// is logged in and not after, because lingering is off, or that a drop-in
// overrides, is Degraded.
func (s Scheduler) Inspect(ctx context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	status := s.Definition(site, ref)
	if !validRef(ref) {
		status.State = scheduler.Unknown
		status.Problem = &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Fix: "Run agent-archive setup"}
		return status
	}
	p := s.probe(ctx, site, ref)
	status.State, status.Problem, status.Degraded = p.state, p.problem, p.degraded
	if p.state.Active() {
		lingerCtx, cancel := context.WithTimeout(ctx, stateTimeout)
		defer cancel()
		if output, err := s.Run(lingerCtx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger"); err == nil && strings.TrimSpace(string(output)) == "Linger=no" {
			status.Degraded = append(status.Degraded, "lingering is off, so the collector runs while you are logged in and stops when you log out; run `loginctl enable-linger` to keep it running")
		}
	}
	return status
}

// systemctl runs `systemctl --user args...` and says what it printed when it
// fails.
func (s Scheduler) systemctl(ctx context.Context, args ...string) error {
	output, err := s.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", args[0], err, output)
	}
	return nil
}

// Load has systemd read the unit files on disk and start the job: it reloads
// the manager (which has not seen files written since it last looked, or the
// removal of ones a rollback deleted), then enables the timer and starts it.
// The timer's first run is at once, since a minute after boot has passed (in
// a container, OnBootSec= counts from the manager's own start instead, so the
// first run may wait up to a minute). A failure is reported as an incomplete setup, with rollback and a retry path.
//
// It runs on a context of its own, bounded by the change timeout, that
// neither ctx's cancellation nor its deadline reaches (context.WithoutCancel):
// an interrupt never stops a change halfway, since the setup journal handles
// what is half applied.
func (s Scheduler) Load(ctx context.Context, _ scheduler.Site, ref scheduler.Ref) error {
	if !validRef(ref) {
		return fmt.Errorf("%q is not a job of this tool", ref)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.changeTimeout())
	defer cancel()
	if err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return s.systemctl(ctx, "enable", "--now", string(ref)+".timer")
}

// Unload stops the job ref names, only when systemd loaded it from this
// site's own unit files, and refuses otherwise with a *scheduler.NotOwnedError
// or a *scheduler.IndeterminateError: nil when it is not loaded. It disables
// the timer and stops it, stops the service (a collection may be running) and
// clears its failed state, and reloads the manager. Files are deleted, when
// they are, by shared code afterwards, and the manager needs no reload for
// that: it unloads an inactive unit that nothing references by itself (seen
// on systemd 239 to 255), and Load reloads before it enables. Like Load,
// nothing of ctx but its values reaches it.
func (s Scheduler) Unload(ctx context.Context, site scheduler.Site, ref scheduler.Ref) error {
	if !validRef(ref) {
		return fmt.Errorf("%q is not a job of this tool", ref)
	}
	ctx = context.WithoutCancel(ctx)
	p := s.probe(ctx, site, ref)
	switch p.state {
	case scheduler.Loaded, scheduler.Running:
	case scheduler.Missing:
		return nil
	case scheduler.AnotherInstallation:
		return &scheduler.NotOwnedError{Words: s.Words(), Problem: *p.problem}
	case scheduler.Unknown:
		fallthrough
	default:
		return &scheduler.IndeterminateError{Words: s.Words(), Problem: *p.problem}
	}
	ctx, cancel := context.WithTimeout(ctx, s.changeTimeout())
	defer cancel()
	if p.timer.exists() {
		if err := s.systemctl(ctx, "disable", "--now", string(ref)+".timer"); err != nil {
			return err
		}
	}
	if p.service.exists() {
		if err := s.systemctl(ctx, "stop", string(ref)+".service"); err != nil {
			return err
		}
		// A run the stop ended (or a last run that failed) leaves the service
		// failed, which keeps it loaded and the whole user manager
		// "degraded" after the job is gone. Clearing it is tidying, not the
		// change: systemctl refuses a unit the manager has already unloaded.
		_ = s.systemctl(ctx, "reset-failed", string(ref)+".service")
	}
	return s.systemctl(ctx, "daemon-reload")
}
