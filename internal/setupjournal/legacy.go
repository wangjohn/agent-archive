package setupjournal

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

// LegacyLaunchLabel is the launchd label of the prototype's upload job,
// which the default installation's setup retires.
const LegacyLaunchLabel = "com.agent-skills.skill-runs-upload"

// LegacyJob is a job a setup retires: the prototype's upload job, or a
// collector an earlier release installed under another label. Change holds
// its plist as setup found it.
type LegacyJob struct {
	Change    hooks.Change `json:"change"`
	WasLoaded bool         `json:"was_loaded"`
}

// PlanLegacyMigration prepares retiring the prototype's upload job in
// userHome, or returns nil when there is none. Only the prototype's exact
// label and command shape establish ownership. Do not execute the plist or
// remove the private records referenced by --home.
func PlanLegacyMigration(userHome string, launchd Launchd) (*LegacyJob, error) {
	path := filepath.Join(userHome, "Library", "LaunchAgents", LegacyLaunchLabel+".plist")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var key, label string
	var args []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot verify legacy upload job ownership: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		//lint:ignore LV1001 XML element names from a launchd plist, an external format
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, err
			}
			if key == "Label" {
				label = value
			}
			key = ""
		case "array":
			if key == "ProgramArguments" {
				var a struct {
					Values []string `xml:"string"`
				}
				if err := decoder.DecodeElement(&a, &start); err != nil {
					return nil, err
				}
				args = a.Values
				key = ""
			}
		}
	}
	if label != LegacyLaunchLabel || len(args) != 5 || filepath.Base(args[1]) != "skill_runs.py" || args[2] != "--home" || args[3] == "" || args[4] != "upload" {
		return nil, fmt.Errorf("legacy job path contains an unrecognized command; preserve %s and resolve it before setup", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Only an existing, recognized prototype job needs launchctl to answer:
	// retiring it without knowing whether it is loaded could leave the
	// prototype uploader running with its plist gone. With no legacy plist
	// on disk (the common fresh install) this function returned nil above,
	// so an unknown launchctl state never blocks setup, matching applySetup's
	// tolerance for the main job on a fresh install.
	state := launchd.JobState(path)
	if state == "unknown" {
		return nil, fmt.Errorf("cannot determine legacy upload job state; restore launchctl access and retry")
	}
	if state == JobAnotherInstallation {
		return nil, fmt.Errorf("launchd's legacy upload job was loaded from a plist other than %s; preserve it and resolve it before setup", path)
	}
	return &LegacyJob{Change: hooks.Change{Path: path, Before: data, Existed: true, Mode: info.Mode().Perm()}, WasLoaded: JobActive(state)}, nil
}

func retireLegacyJob(job *LegacyJob, launchd Launchd) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, job.Change.Before) {
		return fmt.Errorf("legacy upload job changed during setup; retry")
	}
	if job.WasLoaded {
		if err := launchd.Unload(job.Change.Path); err != nil {
			return err
		}
	}
	return os.Remove(job.Change.Path)
}

// restoreLegacyJob puts back a job setup retired: the legacy upload job, or
// a collector an earlier release installed under another label. name
// says which in errors, and home is the data directory whose interrupted
// setup is being recovered.
func restoreLegacyJob(home string, job *LegacyJob, name string, launchd Launchd) error {
	if job == nil {
		return nil
	}
	if err := checkLegacyJob(home, job, name); err != nil {
		return err
	}
	if _, err := os.Stat(job.Change.Path); os.IsNotExist(err) {
		if err := hooks.Apply([]hooks.Change{{Path: job.Change.Path, After: job.Change.Before, Mode: job.Change.Mode}}); err != nil {
			return err
		}
	}
	if job.WasLoaded {
		state := launchd.JobState(job.Change.Path)
		//lint:ignore LV1001 Launchd.JobState reports launchd states as plain strings, and tests stub it with string-returning funcs
		switch state {
		case "unknown":
			return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the state of the %s is unknown; restore access to launchctl and rerun setup", name)}
		case JobAnotherInstallation:
			return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("launchd runs the %s's label from another plist now, so it cannot be restarted from %s", name, job.Change.Path)}
		case "loaded", "running":
		default:
			if err := launchd.Load(job.Change.Path); err != nil {
				return launchctlBlocked(home, "restart the "+name, err)
			}
		}
	}
	return nil
}

// checkLegacyJob confirms restoreLegacyJob can put job back: its plist is
// either gone (setup removed it) or exactly as setup found it.
func checkLegacyJob(home string, job *LegacyJob, name string) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, job.Change.Before) {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the %s's plist %s changed outside setup, and recovery never overwrites your edits", name, job.Change.Path)}
	}
	return nil
}
