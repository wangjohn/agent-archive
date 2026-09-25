package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Every local file whose damage can stop a command is either read around
// (it is advisory: status leaves it out with a warning, and its writer
// replaces it) or named, with a way out: the command that reads it offers to
// move it aside, or says how to. Nothing here deletes a damaged file; it is
// moved aside, so whatever it still says can be looked at.

// movedAsideSuffix ends the name of a local file moved aside because it could
// not be read, after the time it was moved (see moveAside). The collector's
// state files use the same suffix (state.ErrQuarantined).
const movedAsideSuffix = ".corrupt"

// moveAside renames the unreadable file at path to path.<time>.corrupt and
// returns the new path.
func moveAside(path string) (string, error) {
	aside := path + "." + time.Now().UTC().Format("20060102T150405Z") + movedAsideSuffix
	if err := os.Rename(path, aside); err != nil {
		return "", fmt.Errorf("move %s aside: %w", path, err)
	}
	return aside, nil
}

// isMovedAside reports whether name is a file moveAside (or the collector's
// quarantine) made of one of the entries in known.
func isMovedAside(name string, known map[string]bool) bool {
	if !strings.HasSuffix(name, movedAsideSuffix) {
		return false
	}
	for entry := range known {
		if strings.HasPrefix(name, entry+".") {
			return true
		}
	}
	return false
}

// unreadableWarning is the warning a command gives when it reads around an
// advisory local file that could not be read: which file, why, and what
// happens to it next.
func unreadableWarning(path string, err error, next string) string {
	return fmt.Sprintf("%s could not be read (%v); status left it out. %s", path, err, next)
}

// statusState is the collector's per-session state as status reads it:
// every registration and request that can be read, and a warning naming each
// one that cannot, instead of failing status as a whole.
func statusState(home string, store *state.Store) (regs []archive.SessionRegistration, reqs []state.Request, warnings []string) {
	for _, dir := range []string{"registrations", "requests"} {
		entries, err := os.ReadDir(filepath.Join(home, dir))
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, unreadableWarning(filepath.Join(home, dir), err, "Sessions recorded there are not counted."))
			}
			continue
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), ".json")
			if entry.IsDir() || !ok {
				continue
			}
			path := filepath.Join(home, dir, entry.Name())
			if dir == "registrations" {
				reg, found, err := store.LoadRegistration(id)
				switch {
				case err != nil:
					warnings = append(warnings, unreadableWarning(path, err, "The next collector pass moves a damaged one aside (agent-archive sync runs one now), and the session's hooks can register it again."))
				case found:
					regs = append(regs, reg)
				}
				continue
			}
			req, found, err := store.LoadRequest(id)
			switch {
			case err != nil:
				warnings = append(warnings, unreadableWarning(path, err, "The next collector pass moves a damaged one aside (agent-archive sync runs one now)."))
			case found:
				reqs = append(reqs, req)
			}
		}
	}
	return regs, reqs, warnings
}
