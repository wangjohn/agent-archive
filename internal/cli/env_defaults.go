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

// loadLaunchAgent loads a just-installed LaunchAgent so scheduled
// collection starts immediately rather than waiting for the next login.
// This shells out to launchctl and has not been verified against a real
// launchd (see docs/agent-archive-implementation.md); a failure here is
// reported as an incomplete setup, with rollback and a retry path.
func loadLaunchAgent(plistPath string) error {
	cmd := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}

// unloadLaunchAgent stops the job a plist defines. It names the job by its
// service target (gui/UID/label), as jobState checks it, rather than by the
// plist: bootout by path needs the file, and fails with a misleading
// "Input/output error" when the plist was deleted while the job stayed
// loaded. Every plist this tool loads is named after its label.
func unloadLaunchAgent(plistPath string) error {
	cmd := exec.Command("launchctl", "bootout", serviceTarget(plistPath))
	output, err := cmd.CombinedOutput()
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "launchctl", "print", serviceTarget(plist)).CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Could not find service") {
			return "missing"
		}
		return "unknown"
	}
	if strings.Contains(string(output), "state = running") {
		return "running"
	}
	return "loaded"
}
