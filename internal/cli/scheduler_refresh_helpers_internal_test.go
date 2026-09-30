package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// The characterization tests of setup --refresh's scheduler paths reach
// launchd only through runLaunchctl, the one seam every scheduler
// implementation keeps, so they need no Env.JobState, LoadLaunchAgent, or
// UnloadLaunchAgent stand-ins. They replace a package variable and so do not
// run in parallel.

// defaultCollectorLabel is the label of the account's default installation's
// collector, which no release has changed.
const defaultCollectorLabel = "com.agent-archive.collector"

// launchdMode is how argvLaunchd answers `launchctl print`.
type launchdMode string

const (
	// modeLoaded: the job is loaded from the collector's plist.
	modeLoaded launchdMode = "loaded"
	// modeMissing: launchd has no such job.
	modeMissing launchdMode = "missing"
	// modeUnknown: launchctl fails without saying the job is missing.
	modeUnknown launchdMode = "unknown"
	// modeElsewhere: the label is loaded from some other plist.
	modeElsewhere launchdMode = "elsewhere"
)

// argvLaunchd is a launchctl that answers as launchd does for one job and
// records every call's arguments, and the time left on each call's context.
// Like launchd, it refuses to bootstrap a label it already runs (from any
// plist) or a plist that is not there, and to boot out a job it does not
// run, so a golden here cannot record a sequence a Mac would not accept.
type argvLaunchd struct {
	mu    sync.Mutex
	mode  launchdMode
	label string
	plist string
	calls []string
	// remaining is the first-seen time left before the context of a call
	// with each verb expires, or noDeadline.
	remaining map[string]time.Duration
	// failBootstrap makes the next bootstrap fail.
	failBootstrap bool
	// hang lists the verbs whose launchctl never answers: it returns only when
	// its context ends, as a launchctl stuck in the kernel is killed.
	hang map[string]bool
}

// stubArgvLaunchd replaces launchctl with an argvLaunchd for the test.
func stubArgvLaunchd(t *testing.T, plist string, mode launchdMode) *argvLaunchd {
	t.Helper()
	l := &argvLaunchd{mode: mode, label: launchLabel(plist), plist: plist, remaining: map[string]time.Duration{}}
	stubLaunchctlContext(t, l.run)
	return l
}

// noDeadline is what remaining records for a call whose context has no
// deadline, told apart from one whose deadline had already passed.
const noDeadline = time.Duration(math.MinInt64)

func (l *argvLaunchd) run(ctx context.Context, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, strings.Join(args, " "))
	if _, seen := l.remaining[args[0]]; !seen {
		l.remaining[args[0]] = noDeadline
		if deadline, ok := ctx.Deadline(); ok {
			l.remaining[args[0]] = time.Until(deadline)
		}
	}
	if l.hang[args[0]] {
		l.mu.Unlock()
		<-ctx.Done()
		l.mu.Lock()
		return nil, ctx.Err()
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	switch {
	case len(args) == 2 && args[0] == "print" && args[1] == domain+"/"+l.label:
		switch l.mode {
		case modeLoaded:
			return []byte(args[1] + " = {\n\tpath = " + l.plist + "\n\tstate = running\n}\n"), nil
		case modeElsewhere:
			return []byte(args[1] + " = {\n\tpath = /somewhere/else.plist\n\tstate = running\n}\n"), nil
		case modeUnknown:
			return []byte("launchctl: something went wrong"), errors.New("exit status 1")
		case modeMissing:
		}
	case len(args) == 2 && args[0] == "print" && strings.HasPrefix(args[1], domain+"/"):
		// Any other label is not loaded.
	case len(args) == 2 && args[0] == "bootout" && args[1] == domain+"/"+l.label:
		switch l.mode {
		case modeLoaded, modeElsewhere:
			l.mode = modeMissing
			return nil, nil
		case modeUnknown:
			return []byte("launchctl: something went wrong"), errors.New("exit status 1")
		case modeMissing:
		}
		return []byte("Boot-out failed: 113: Could not find specified service"), errors.New("exit status 113")
	case len(args) == 3 && args[0] == "bootstrap" && args[1] == domain && launchLabel(args[2]) == l.label:
		_, statErr := os.Stat(args[2])
		if l.failBootstrap || l.mode != modeMissing || statErr != nil {
			l.failBootstrap = false
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		l.mode, l.plist = modeLoaded, args[2]
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected launchctl %v", args)
	}
	return []byte("Bad request.\nCould not find service \"" + path.Base(args[1]) + "\" in domain for user gui: " + strconv.Itoa(os.Getuid()) + "\n"), errors.New("exit status 113")
}

// argv is the calls so far, each as its arguments joined by spaces.
func (l *argvLaunchd) argv() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// label is the collector's launchd label: its plist's file name.
func (r *refreshInstall) label() string { return launchLabel(r.plist) }

// print, bootout and bootstrap are the launchctl arguments, joined, that ask
// about, stop and load the collector's job in this user's session.
func (r *refreshInstall) print() string {
	return fmt.Sprintf("print gui/%d/%s", os.Getuid(), r.label())
}

func (r *refreshInstall) bootout() string {
	return fmt.Sprintf("bootout gui/%d/%s", os.Getuid(), r.label())
}

func (r *refreshInstall) bootstrap() string {
	return fmt.Sprintf("bootstrap gui/%d %s", os.Getuid(), r.plist)
}

// refreshInstall is a set-up installation whose executable then changes (when
// upgrade is set), on a launchctl that answers in a mode.
type refreshInstall struct {
	home     string
	userHome string
	env      Env
	oldExe   string
	newExe   string
	plist    string
	launchd  *argvLaunchd
}

// newRefreshInstall runs setup (Claude Code and Codex, every skill) for the
// account's default installation or for a data directory elsewhere, then
// leaves launchd to the argvLaunchd stand-in. With upgrade, the running
// executable is then another one.
func newRefreshInstall(t *testing.T, defaultInstall, upgrade bool, mode launchdMode) *refreshInstall {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	if defaultInstall {
		home = filepath.Join(userHome, ".local", "share", "agent-archive")
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if defaultInstall {
		env.AccountHome = func() (string, error) { return userHome, nil }
	}
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	r := &refreshInstall{home: home, userHome: userHome, env: env, oldExe: mustLoadConfig(t, home).InstalledExecutable}
	r.newExe = r.oldExe
	if upgrade {
		r.newExe = upgradedTo(t, &r.env)
	}
	r.env.JobState, r.env.LoadLaunchAgent, r.env.UnloadLaunchAgent = nil, nil, nil
	r.plist = r.env.installation(home, userHome).collectorPlist()
	r.launchd = stubArgvLaunchd(t, r.plist, mode)
	return r
}

// paths are the places a refresh's files name, as tokens for goldens.
func (r *refreshInstall) paths() *strings.Replacer {
	pairs := map[string]string{
		r.home:                          "@DATA_HOME@",
		local.CanonicalPath(r.home):     "@DATA_HOME@",
		r.userHome:                      "@USER_HOME@",
		local.CanonicalPath(r.userHome): "@USER_HOME@",
		r.oldExe:                        "@OLD_EXE@",
		r.newExe:                        "@EXE@",
	}
	if label := r.label(); label != defaultCollectorLabel {
		pairs[label] = "@LABEL@"
	}
	// The longest first: a replacer prefers the earliest argument at a spot,
	// and a path can be the start of another.
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
	var args []string
	for _, k := range keys {
		args = append(args, k, pairs[k])
	}
	return strings.NewReplacer(args...)
}

// expand is a fixture text with its tokens filled in for this installation.
func (r *refreshInstall) expand(text string) string {
	return strings.NewReplacer("@DATA_HOME@", r.home, "@OLD_EXE@", r.oldExe, "@EXE@", r.newExe, "@LABEL@", r.label()).Replace(text)
}

// tokenize is text with this installation's paths as tokens, for a golden.
func (r *refreshInstall) tokenize(text string) string { return r.paths().Replace(text) }

// run is setup --refresh, as the installer runs it.
func (r *refreshInstall) run(t *testing.T) (code int, stdout, stderr string) {
	t.Helper()
	return refreshRun(t, r.env)
}

// writePlist puts data where the collector's plist is.
func (r *refreshInstall) writePlist(t *testing.T, data string) {
	t.Helper()
	must(t, os.WriteFile(r.plist, []byte(data), 0o600))
}
