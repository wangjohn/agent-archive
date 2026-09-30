// Package systemd is the Linux scheduler: the user's own systemd manager
// (systemctl --user), which runs the collector from a timer. It holds
// everything that knows systemd's vocabulary: the two unit files a job is
// (a oneshot service and the timer that starts it, written and read in
// unit.go), the unit names that are the jobs' refs, and the systemctl calls
// that ask about, load and stop a job (manage.go).
//
// It is pure over the scheduler.Runner it is given and the environment
// lookup: it imports no os/exec, and constructing a Scheduler runs nothing.
package systemd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// DefaultPATH is the PATH systemd's own compiled-in default gives a service
// that sets none: /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin, with
// /sbin:/bin after them (systemd.exec(5), $PATH; the last two are the
// default of a build for a system whose /bin and /sbin are not links into
// /usr, and harmless on one where they are). It is what a user manager
// started from nothing has: in practice the user manager inherits the
// login's PATH, which differs, so this is the value the collector's
// environment can rely on and no more. It is taken from the documentation
// and not read from a live manager; the Linux job environment work (5c)
// revisits it.
const DefaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// collectorRef is the unit name (without .service or .timer) of the default
// installation's collector; another installation's is followed by "-" and the
// first 12 hex digits of the SHA-256 of its data directory, the digest the
// launchd label uses.
const collectorRef = "agent-archive-collector"

// collectorInterval is how often the collector's job runs.
const collectorInterval = time.Minute

// Scheduler is the user's systemd manager, through systemctl. A job's
// definition is two files in the user unit directory (see UnitDir):
// <ref>.service and <ref>.timer.
//
// It runs systemctl and loginctl only through Run, and builds nothing that
// runs them: making a Scheduler executes no program.
type Scheduler struct {
	// Run runs a program, and is required.
	Run scheduler.Runner
	// Getenv reads the process's environment; nil reads nothing. The adapter
	// reads two variables: XDG_CONFIG_HOME, where systemd looks for a user's
	// units when it is set to an absolute path, and HOME, which tells whether
	// that is the site's own home (see UnitDir).
	Getenv func(string) string
}

// Name is "systemd", which the setup journal records for the jobs it made.
func (Scheduler) Name() string { return "systemd" }

// Words are systemd's nouns.
func (Scheduler) Words() scheduler.Words {
	return scheduler.Words{Manager: "systemd", Job: "user timer", Definition: "unit file", Tool: "systemctl"}
}

// DefaultPATH is the PATH systemd gives a service that sets none.
func (Scheduler) DefaultPATH() string { return DefaultPATH }

// Ref is the unit name of the installation's collector: agent-archive-collector
// for the account's default installation, and otherwise that followed by "-"
// and the first 12 hex digits of the SHA-256 of the cleaned data directory
// (the same digest launchd's CollectorLabel takes).
func (Scheduler) Ref(inst scheduler.Installation) scheduler.Ref {
	if inst.Default {
		return collectorRef
	}
	sum := sha256.Sum256([]byte(filepath.Clean(inst.DataHome)))
	return collectorRef + "-" + scheduler.Ref(hex.EncodeToString(sum[:])[:12])
}

// validRef reports whether ref is a name Ref could have made: a ref reaches
// file paths and systemctl's arguments, so nothing else is accepted.
func validRef(ref scheduler.Ref) bool {
	suffix, ok := strings.CutPrefix(string(ref), collectorRef)
	if !ok {
		return false
	}
	if suffix == "" {
		return true
	}
	digits, ok := strings.CutPrefix(suffix, "-")
	return ok && len(digits) == 12 && strings.Trim(digits, "0123456789abcdef") == ""
}

func (s Scheduler) getenv(name string) string {
	if s.Getenv == nil {
		return ""
	}
	return s.Getenv(name)
}

// UnitDir is where systemd looks for the site's user's own units:
// $XDG_CONFIG_HOME/systemd/user when the process's XDG_CONFIG_HOME is an
// absolute path, and <user home>/.config/systemd/user otherwise, which is the
// rule systemd's user manager applies to its own environment. The variable is
// honored only for the site whose UserHome is the process's own HOME, since it
// describes that user's environment: a sandbox that overrides HOME and a test
// that names another home keep their units under it, never in the real
// configuration directory. (The manager reads its own environment, which a
// login shell's export may not have reached: when they differ, Load fails
// with systemctl's "unit file does not exist", and setup rolls back.)
func (s Scheduler) UnitDir(site scheduler.Site) string {
	config, home := s.getenv("XDG_CONFIG_HOME"), s.getenv("HOME")
	if filepath.IsAbs(config) && home != "" && filepath.Clean(home) == filepath.Clean(site.UserHome) {
		return filepath.Join(config, "systemd", "user")
	}
	return filepath.Join(site.UserHome, ".config", "systemd", "user")
}

func (s Scheduler) servicePath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(s.UnitDir(site), string(ref)+".service")
}

func (s Scheduler) timerPath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(s.UnitDir(site), string(ref)+".timer")
}

// Locate is the site and the job of a unit file path, the service's or the
// timer's: <unit directory>/<ref>.service, where the unit directory is the
// one UnitDir gives for the site (the process's own home is tried first, then
// the home a <home>/.config/systemd/user path names). A path no Plan could
// have written, here and now, is refused rather than taken for another unit:
// a journal recorded while XDG_CONFIG_HOME was different is left as it is.
func (s Scheduler) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	refuse := fmt.Errorf("%s is not a unit file of this tool in the user unit directory (<config home>/systemd/user/%s[-<12 hex digits>].service or .timer); systemd was left as it is", definition, collectorRef)
	stem, ok := strings.CutSuffix(definition, ".service")
	if !ok {
		stem, ok = strings.CutSuffix(definition, ".timer")
	}
	if !ok || !filepath.IsAbs(definition) || filepath.Clean(definition) != definition {
		return scheduler.Site{}, "", refuse
	}
	dir, ref := filepath.Split(stem)
	dir = filepath.Clean(dir)
	if !validRef(scheduler.Ref(ref)) {
		return scheduler.Site{}, "", refuse
	}
	homes := []string{s.getenv("HOME")}
	if config := filepath.Join(".config", "systemd", "user"); strings.HasSuffix(dir, string(filepath.Separator)+config) {
		homes = append(homes, strings.TrimSuffix(dir, string(filepath.Separator)+config))
	}
	for _, home := range homes {
		if site := (scheduler.Site{UserHome: home}); home != "" && s.UnitDir(site) == dir {
			return site, scheduler.Ref(ref), nil
		}
	}
	return scheduler.Site{}, "", refuse
}

// Plan is the collector's two unit files for inst at site, as artifacts:
// <ref>.service, then <ref>.timer, in the unit directory. It reads nothing
// and asks systemd nothing. Only the collector's job is defined: it runs the
// executable with `_collect` every minute, at load too, so a spec that says
// otherwise is refused rather than rendered as something else.
//
// The files are private (0600), as launchd's plists are, though the unit
// convention is 0644: they hold no credential, but the environment may hold
// a proxy address with its user name and password in it, and the user manager,
// which runs as the user, reads them either way.
func (s Scheduler) Plan(site scheduler.Site, inst scheduler.Installation, spec scheduler.JobSpec) (scheduler.Plan, error) {
	if !slices.Equal(spec.Args, []string{"_collect"}) || spec.Interval != collectorInterval || !spec.RunAtLoad {
		return scheduler.Plan{}, errors.New("a systemd user timer runs the collector: _collect every minute, at load")
	}
	ref := s.Ref(inst)
	service, err := renderService(spec.Executable, spec.DataHome, spec.Env)
	if err != nil {
		return scheduler.Plan{}, err
	}
	return scheduler.Plan{Ref: ref, Artifacts: []scheduler.Artifact{
		scheduler.FileArtifact(s.servicePath(site, ref), service, 0o600),
		scheduler.FileArtifact(s.timerPath(site, ref), renderTimer(string(ref)), 0o600),
	}}, nil
}

// Installed is the installation's own job: systemd has no earlier names to
// look for, so there are no aliases, and nothing is asked of the disk or the
// manager.
func (s Scheduler) Installed(_ context.Context, _ scheduler.Site, inst scheduler.Installation) ([]scheduler.Job, error) {
	return []scheduler.Job{{Ref: s.Ref(inst)}}, nil
}

// Definition reads the job's unit files alone, never the manager's view of
// them: whether they are there, the program the service runs and the
// environment it sets. `systemctl show` would fold in a drop-in's settings,
// which refresh must not bake into a new unit. A service that cannot be read,
// or has no program to read, is reported in DefinitionErr, with what could be
// read still set. A timer with no service is a definition with no program.
func (s Scheduler) Definition(site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	if !validRef(ref) {
		return scheduler.Status{DefinitionErr: fmt.Errorf("%q is not a job of this tool", ref)}
	}
	servicePath, timerPath := s.servicePath(site, ref), s.timerPath(site, ref)
	status := scheduler.Status{Paths: []string{servicePath, timerPath}}
	data, err := os.ReadFile(servicePath)
	_, timerErr := os.Stat(timerPath)
	serviceAbsent := errors.Is(err, os.ErrNotExist)
	if serviceAbsent && errors.Is(timerErr, os.ErrNotExist) {
		return status
	}
	status.Defined = true
	switch {
	case serviceAbsent:
		status.DefinitionErr = fmt.Errorf("the service unit %s is missing", servicePath)
	case err != nil:
		status.DefinitionErr = err
	default:
		program, environment, readErr := readService(data)
		status.DefinitionErr = readErr
		if readErr == nil {
			status.Program, status.DataHome = program, environment["AGENT_ARCHIVE_HOME"]
			delete(environment, "AGENT_ARCHIVE_HOME")
			status.Env = environment
		}
	}
	return status
}
