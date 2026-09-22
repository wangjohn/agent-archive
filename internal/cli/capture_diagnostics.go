package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

const (
	diagnosticUnknownSessionStart = "session_start_unknown"
	diagnosticPreActivationStart  = "session_started_before_activation"
	// diagnosticSetupInProgress records a start that arrived while setup's own
	// transaction was open. Hooks do not register anything in that window.
	diagnosticSetupInProgress = "setup_in_progress"
)

// captureDiagnostic is deliberately content-free. It records only the
// integration boundary that prevented capture; native session identifiers,
// transcript paths, hook payloads, and conversation content never belong here.
type captureDiagnostic struct {
	Code        string    `json:"code"`
	Harness     string    `json:"harness"`
	ProjectRoot string    `json:"project_root,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
}

func captureDiagnosticsPath(home string) string {
	return filepath.Join(home, "capture-diagnostics.json")
}

func readCaptureDiagnostics(home string) ([]captureDiagnostic, error) {
	var diagnostics []captureDiagnostic
	if err := local.Read(captureDiagnosticsPath(home), &diagnostics); err != nil {
		if os.IsNotExist(err) {
			return []captureDiagnostic{}, nil
		}
		return nil, err
	}
	return diagnostics, nil
}

const (
	// diagnosticsLockName serializes every read-modify-write of
	// capture-diagnostics.json: each hook-side record and setup's prune.
	diagnosticsLockName = "diagnostics.lock"
	// pruneDiagnosticsWait is setup's wait. Holders keep the lock for one
	// small file write, so this only has to outlast a burst of hooks.
	pruneDiagnosticsWait = 2 * time.Second
)

// hookDiagnosticsWait bounds how long a hook waits for diagnostics.lock. A
// diagnostic is advisory, and a hook runs on the user's turn, so on timeout
// the diagnostic is dropped rather than the turn delayed. A variable only so
// a race test can rule out timeout drops and observe lost updates alone.
var hookDiagnosticsWait = 50 * time.Millisecond

// recordCaptureDiagnostic adds a diagnostic under diagnostics.lock, or drops it
// if the lock is not free within hookDiagnosticsWait. Under the lock it rereads
// the configuration: the caller decided the project was included from a
// snapshot, and setup may have excluded it and pruned its diagnostics since.
// Checking against the committed configuration, under the same lock the prune
// takes, means a pruned project's diagnostic can never come back, whichever
// of the two runs first.
func recordCaptureDiagnostic(home string, diagnostic captureDiagnostic) error {
	unlock, err := local.NamedLockWait(home, diagnosticsLockName, hookDiagnosticsWait)
	if errors.Is(err, local.ErrBusy) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock capture diagnostics: %w", err)
	}
	defer unlock()
	cfg, found, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found || len(includedCaptureDiagnostics([]captureDiagnostic{diagnostic}, cfg.Archive.Projects)) == 0 {
		return nil
	}
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil {
		return err
	}
	diagnostic.ObservedAt = diagnostic.ObservedAt.UTC()
	// Keep only the latest instance of a reason for an app/project pair. This
	// bounds local status data even when a harness repeats the same hook.
	kept := diagnostics[:0]
	for _, existing := range diagnostics {
		if existing.Code == diagnostic.Code && existing.Harness == diagnostic.Harness && existing.ProjectRoot == diagnostic.ProjectRoot {
			continue
		}
		kept = append(kept, existing)
	}
	kept = append(kept, diagnostic)
	sort.Slice(kept, func(i, j int) bool { return kept[i].ObservedAt.Before(kept[j].ObservedAt) })
	if len(kept) > 50 {
		kept = kept[len(kept)-50:]
	}
	return local.Write(captureDiagnosticsPath(home), kept)
}

// includedCaptureDiagnostics keeps only diagnostics for projects that are
// currently included. A diagnostic is recorded only for an included project,
// but the project may be excluded later; its path must then stop appearing
// in status, not linger until newer entries push it out.
func includedCaptureDiagnostics(diagnostics []captureDiagnostic, projects []archive.ProjectActivation) []captureDiagnostic {
	kept := make([]captureDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		for _, project := range projects {
			if project.Included && filepath.Clean(project.Root) == filepath.Clean(diagnostic.ProjectRoot) {
				kept = append(kept, diagnostic)
				break
			}
		}
	}
	return kept
}

// pruneCaptureDiagnostics drops stored diagnostics for projects that are no
// longer included, so an excluded path is not kept on disk either. Setup
// calls it after committing the configuration those projects come from; see
// recordCaptureDiagnostic for why that order plus the shared lock is enough.
func pruneCaptureDiagnostics(home string, projects []archive.ProjectActivation) error {
	unlock, err := local.NamedLockWait(home, diagnosticsLockName, pruneDiagnosticsWait)
	if err != nil {
		return fmt.Errorf("lock capture diagnostics: %w", err)
	}
	defer unlock()
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil {
		return err
	}
	kept := includedCaptureDiagnostics(diagnostics, projects)
	if len(kept) == len(diagnostics) {
		return nil
	}
	return local.Write(captureDiagnosticsPath(home), kept)
}

func captureDiagnosticMessage(code string) string {
	switch code {
	case diagnosticUnknownSessionStart:
		return "the session start could not be established"
	case diagnosticPreActivationStart:
		return "the session start does not meet the project activation boundary"
	case diagnosticSetupInProgress:
		return "setup was still in progress, so the session was not registered; start a new session"
	default:
		return "capture evidence was not accepted"
	}
}
