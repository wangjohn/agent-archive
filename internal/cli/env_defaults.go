package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

// detectHarnesses best-effort-detects installed applications by checking
// for the configuration directory holding each app's hook file (see
// hooks.ResolveFiles). A directory existing is not proof the application is
// currently installed, and its absence is not proof it isn't; this only
// pre-selects setup's prompts, which the user can override either way.
func detectHarnesses(files hooks.Files) []string {
	var found []string
	for _, name := range allHarnesses {
		if info, err := os.Stat(filepath.Dir(files[name])); err == nil && info.IsDir() {
			found = append(found, name)
		}
	}
	return found
}

// runLaunchctl runs launchctl with args. Tests replace it; nothing else
// shells out to launchctl.
var runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
}

// jobAnotherInstallation is the job state of a label launchd has loaded
// from a plist other than the one asked about: the job belongs to another
// installation (the user's real one, seen from a sandboxed HOME, say), and
// nothing here may stop or replace it.
const jobAnotherInstallation = "another_installation"

// loadLaunchAgent loads a just-installed LaunchAgent so scheduled
// collection starts immediately rather than waiting for the next login.
// It shells out to launchctl (checked against a real launchd in live
// installs); a failure here is reported as an incomplete setup, with
// rollback and a retry path.
func loadLaunchAgent(plistPath string) error {
	output, err := runLaunchctl(context.Background(), "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}

// unloadLaunchAgent stops the job a plist defines. It names the job by its
// service target (gui/UID/label), as jobState checks it, rather than by the
// plist: bootout by path needs the file, and fails with a misleading
// "Input/output error" when the plist was deleted while the job stayed
// loaded. A label alone does not prove ownership, so it first confirms
// launchd loaded the job from plistPath itself, and refuses otherwise.
func unloadLaunchAgent(plistPath string) error {
	//lint:ignore LV1001 Env.JobState (cli.go) reports launchd states as plain strings, and tests stub it with string-returning funcs
	switch state := launchdJobState(plistPath); state {
	case "loaded", "running":
	case "missing":
		return nil
	case jobAnotherInstallation:
		return fmt.Errorf("launchd's %s job was not loaded from %s; it belongs to another installation and was left running", launchLabel(plistPath), plistPath)
	default:
		return fmt.Errorf("cannot confirm which plist launchd's %s job was loaded from; it was left as it is", launchLabel(plistPath))
	}
	output, err := runLaunchctl(context.Background(), "bootout", serviceTarget(plistPath))
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, output)
	}
	return nil
}

// serviceTarget is launchd's name for the job a plist defines in this
// user's GUI session.
func serviceTarget(plist string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel(plist))
}

func (e Env) jobState(plist string) string {
	if e.JobState != nil {
		return e.JobState(plist)
	}
	// Injected schedulers are not the user's launchd.
	if e.LoadLaunchAgent != nil {
		return "missing"
	}
	return launchdJobState(plist)
}

// launchdJobState asks launchd about the job plist defines, by its label.
func launchdJobState(plist string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := runLaunchctl(ctx, "print", serviceTarget(plist))
	return parseJobState(string(output), err, plist)
}

// parseJobState reads `launchctl print` output for the job plist defines:
// missing, loaded, or running when launchd loaded the label from plist
// itself; jobAnotherInstallation when it loaded it from another file; and
// unknown when launchctl fails or names no file to compare.
func parseJobState(output string, err error, plist string) string {
	if err != nil {
		if strings.Contains(output, "Could not find service") {
			return "missing"
		}
		return "unknown"
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
		return "unknown"
	case canonicalPath(loadedFrom) != canonicalPath(plist):
		return jobAnotherInstallation
	case strings.Contains(output, "state = running"):
		return "running"
	}
	return "loaded"
}
