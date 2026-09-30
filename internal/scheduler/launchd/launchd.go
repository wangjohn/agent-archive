package launchd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// ChangeTimeout bounds launchctl bootstrap and bootout. Setup --refresh
// absorbs Ctrl-C and SIGTERM while it stops and starts the job, so a launchctl
// that hangs must end on its own: the transaction then fails and rolls back
// (or, when launchctl stays hung, leaves its journal for the next setup),
// instead of a process nothing but SIGKILL can stop.
const ChangeTimeout = 30 * time.Second

// stateTimeout bounds launchctl print, which only asks.
const stateTimeout = 2 * time.Second

// Scheduler is launchd, through launchctl. A job's definition is
// <user home>/Library/LaunchAgents/<label>.plist, which is where every
// collector plist, earlier release's plist and the prototype's job lives
// (PlistPath).
//
// It runs launchctl only through Run, and builds nothing that runs it: making
// a Scheduler executes no program.
type Scheduler struct {
	// Run runs launchctl, and is required.
	Run scheduler.Runner
	// ChangeTimeout is the bound on bootstrap and bootout; zero means the
	// package's ChangeTimeout. A test shortens it.
	ChangeTimeout time.Duration
}

// JobState asks launchd about the job ref names, by its label, and reads
// launchctl's answer against the plist at site (see ParseJobState).
func (s Scheduler) JobState(ctx context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	return s.jobState(ctx, PlistPath(site, ref))
}

// Load loads the plist of the job ref names into this user's GUI session so
// scheduled collection starts immediately rather than waiting for the next
// login. A failure is reported as an incomplete setup, with rollback and a
// retry path.
//
// It runs on a context of its own, bounded by the change timeout, that
// neither ctx's cancellation nor its deadline reaches (context.WithoutCancel):
// an interrupt never stops a change halfway, since the setup journal handles
// what is half applied.
func (s Scheduler) Load(ctx context.Context, site scheduler.Site, ref scheduler.Ref) error {
	plist := PlistPath(site, ref)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.changeTimeout())
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}

// Unload stops the job the plist of ref defines. It names the job by its
// service target (gui/UID/label), as JobState checks it, rather than by the
// plist: bootout by path needs the file, and fails with a misleading
// "Input/output error" when the plist was deleted while the job stayed
// loaded. A label alone does not prove ownership, so it first confirms
// launchd loaded the job from the plist itself, and refuses otherwise. Like
// Load, nothing of ctx but its values reaches it.
func (s Scheduler) Unload(ctx context.Context, site scheduler.Site, ref scheduler.Ref) error {
	plist := PlistPath(site, ref)
	ctx = context.WithoutCancel(ctx)
	switch state := s.jobState(ctx, plist); state {
	case scheduler.Loaded, scheduler.Running:
	case scheduler.Missing:
		return nil
	case scheduler.AnotherInstallation:
		return fmt.Errorf("launchd's %s job was not loaded from %s; it belongs to another installation and was left running", Label(plist), plist)
	case scheduler.Unknown:
		fallthrough
	default:
		return fmt.Errorf("cannot confirm which plist launchd's %s job was loaded from; it was left as it is", Label(plist))
	}
	ctx, cancel := context.WithTimeout(ctx, s.changeTimeout())
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "bootout", ServiceTarget(plist))
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, output)
	}
	return nil
}

func (s Scheduler) changeTimeout() time.Duration {
	if s.ChangeTimeout > 0 {
		return s.ChangeTimeout
	}
	return ChangeTimeout
}

func (s Scheduler) jobState(ctx context.Context, plist string) scheduler.JobState {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	output, err := s.Run(ctx, "launchctl", "print", ServiceTarget(plist))
	return ParseJobState(string(output), err, plist)
}

// PlistPath is the plist that defines the job ref names at site.
func PlistPath(site scheduler.Site, ref scheduler.Ref) string {
	return filepath.Join(site.UserHome, "Library", "LaunchAgents", string(ref)+".plist")
}

// Label is the launchd label of the job a plist defines. Every job this tool
// loads is named after its label, so the file name is the label.
func Label(plist string) string {
	return strings.TrimSuffix(filepath.Base(plist), ".plist")
}

// ServiceTarget is launchd's name for the job a plist defines in this user's
// GUI session.
func ServiceTarget(plist string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), Label(plist))
}

// ParseJobState reads `launchctl print` output (and the error it failed with,
// if it did) for the job plist defines: Missing, Loaded, or Running when
// launchd loaded the label from plist itself; AnotherInstallation when it
// loaded it from another file; and Unknown when launchctl fails or names no
// file to compare.
func ParseJobState(output string, err error, plist string) scheduler.JobState {
	if err != nil {
		if strings.Contains(output, "Could not find service") {
			return scheduler.Missing
		}
		return scheduler.Unknown
	}
	loadedFrom := ""
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "path = "); ok {
			loadedFrom = strings.TrimSpace(value)
			break
		}
	}
	switch {
	case loadedFrom == "":
		return scheduler.Unknown
	case !local.SameLocation(loadedFrom, plist):
		return scheduler.AnotherInstallation
	case strings.Contains(output, "state = running"):
		return scheduler.Running
	}
	return scheduler.Loaded
}
