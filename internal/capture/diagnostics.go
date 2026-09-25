package capture

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
	"github.com/wangjohn/agent-archive/internal/state"
)

// DiagnosticCode names the boundary that prevented a capture.
type DiagnosticCode string

// The diagnostic codes status explains (DiagnosticMessage).
const (
	DiagnosticUnknownSessionStart DiagnosticCode = "session_start_unknown"
	DiagnosticPreActivationStart  DiagnosticCode = "session_started_before_activation"
	// DiagnosticSetupInProgress records a start that arrived while setup's own
	// transaction was open. Hooks do not register anything in that window.
	DiagnosticSetupInProgress DiagnosticCode = "setup_in_progress"
	// DiagnosticHookFailed records a hook that stopped on an internal error
	// (a recovered panic), so the event it carried was not recorded.
	DiagnosticHookFailed DiagnosticCode = "hook_failed"
)

// Diagnostic is deliberately content-free. It records only the
// integration boundary that prevented capture; native session identifiers,
// transcript paths, hook payloads, and conversation content never belong here.
type Diagnostic struct {
	Code        DiagnosticCode `json:"code"`
	Harness     string         `json:"harness"`
	ProjectRoot string         `json:"project_root,omitempty"`
	ObservedAt  time.Time      `json:"observed_at"`
}

// DiagnosticsPath is capture-diagnostics.json in the data directory home.
func DiagnosticsPath(home string) string {
	return filepath.Join(home, "capture-diagnostics.json")
}

// ReadDiagnostics returns the stored diagnostics, oldest first, or none when
// the file does not exist yet.
func ReadDiagnostics(home string) ([]Diagnostic, error) {
	var diagnostics []Diagnostic
	if err := local.Read(DiagnosticsPath(home), &diagnostics); err != nil {
		if os.IsNotExist(err) {
			return []Diagnostic{}, nil
		}
		return nil, err
	}
	return diagnostics, nil
}

const (
	// DiagnosticsLockName serializes every read-modify-write of
	// capture-diagnostics.json: each hook-side record and setup's prune.
	DiagnosticsLockName = "diagnostics.lock"
	// pruneDiagnosticsWait is setup's wait. Holders keep the lock for one
	// small file write, so this only has to outlast a burst of hooks.
	pruneDiagnosticsWait = 2 * time.Second
)

// hookDiagnosticsWait bounds how long a hook waits for diagnostics.lock. A
// diagnostic is advisory, and a hook runs on the user's turn, so on timeout
// the diagnostic is dropped rather than the turn delayed. A variable only so
// a race test can rule out timeout drops and observe lost updates alone.
var hookDiagnosticsWait = 50 * time.Millisecond

// RecordDiagnostic adds a diagnostic under diagnostics.lock, or drops it
// if the lock is not free within hookDiagnosticsWait. Under the lock it rereads
// the configuration: the caller decided the project was included from a
// snapshot, and setup may have excluded it and pruned its diagnostics since.
// Checking against the committed configuration, under the same lock the prune
// takes, means a pruned project's diagnostic can never come back, whichever
// of the two runs first.
func RecordDiagnostic(home string, diagnostic Diagnostic) error {
	unlock, err := local.NamedLockWait(home, DiagnosticsLockName, hookDiagnosticsWait)
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
	if !found || len(IncludedDiagnostics([]Diagnostic{diagnostic}, cfg.Archive.Projects)) == 0 {
		return nil
	}
	diagnostics, err := ReadDiagnostics(home)
	if state.IsUndecodable(err) {
		// Advisory, and rewritten whole below: a file that no longer decodes
		// is replaced rather than left to fail every later diagnostic.
		diagnostics, err = nil, nil
	}
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
	return local.Write(DiagnosticsPath(home), kept)
}

// IncludedDiagnostics keeps only diagnostics for projects that are
// currently included. A diagnostic is recorded only for an included project,
// but the project may be excluded later; its path must then stop appearing
// in status, not linger until newer entries push it out.
func IncludedDiagnostics(diagnostics []Diagnostic, projects []archive.ProjectActivation) []Diagnostic {
	kept := make([]Diagnostic, 0, len(diagnostics))
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

// PruneDiagnostics drops stored diagnostics for projects that are no
// longer included, so an excluded path is not kept on disk either. Setup
// calls it after committing the configuration those projects come from; see
// RecordDiagnostic for why that order plus the shared lock is enough.
func PruneDiagnostics(home string, projects []archive.ProjectActivation) error {
	unlock, err := local.NamedLockWait(home, DiagnosticsLockName, pruneDiagnosticsWait)
	if err != nil {
		return fmt.Errorf("lock capture diagnostics: %w", err)
	}
	defer unlock()
	diagnostics, err := ReadDiagnostics(home)
	if state.IsUndecodable(err) {
		// Replaced by an empty list: nothing in it can be pruned, and it
		// must not stop setup.
		return local.Write(DiagnosticsPath(home), []Diagnostic{})
	}
	if err != nil {
		return err
	}
	kept := IncludedDiagnostics(diagnostics, projects)
	if len(kept) == len(diagnostics) {
		return nil
	}
	return local.Write(DiagnosticsPath(home), kept)
}

// DiagnosticMessage says in words what a diagnostic code means.
func DiagnosticMessage(code DiagnosticCode) string {
	switch code {
	case DiagnosticUnknownSessionStart:
		return "the session start could not be established"
	case DiagnosticPreActivationStart:
		return "the session start does not meet the project activation boundary"
	case DiagnosticSetupInProgress:
		return "setup was still in progress, so the session was not registered; start a new session"
	case DiagnosticHookFailed:
		return "a hook stopped on an internal error, so its event was not recorded; please report it"
	default:
		return "capture evidence was not accepted"
	}
}
