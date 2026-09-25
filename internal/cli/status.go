package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type appStatus struct {
	PublishedSessions int       `json:"published_sessions"`
	VerifiedSessions  int       `json:"verified_sessions"`
	Configured        bool      `json:"configured"`
	HookObserved      bool      `json:"hook_observed"`
	CapturedLocally   bool      `json:"captured_locally"`
	Published         bool      `json:"published"`
	ReadBackVerified  bool      `json:"read_back_verified"`
	VerifiedAt        time.Time `json:"verified_at,omitzero"`
	VerificationState string    `json:"verification_state"`
	// VerificationDetail explains a read-back that has not succeeded yet for
	// the current publication: the last error, attempts so far, and when the
	// collector will retry. Empty once every publication is verified.
	VerificationDetail string   `json:"verification_detail,omitempty"`
	Trust              string   `json:"trust"`
	HarnessVersions    []string `json:"observed_harness_versions,omitempty"`
	AdapterVersions    []string `json:"observed_adapter_versions,omitempty"`
	// CaptureGaps lists recorded gaps: sessions whose current transcript can
	// no longer be captured (rewritten or over the size limit), per-session
	// scan issues, and gaps recorded inside captured bundles. A blocked
	// session's last published snapshot, if any, stays retained. These are
	// recorded gaps, not errors.
	CaptureGaps []archive.CaptureGap `json:"capture_gaps,omitempty"`
	// SessionsWithCaptureGaps counts the distinct sessions behind
	// CaptureGaps: one session can record several gaps.
	SessionsWithCaptureGaps int       `json:"sessions_with_capture_gaps,omitempty"`
	Installed               bool      `json:"installed"`
	InstalledVersion        string    `json:"installed_version,omitempty"`
	VersionSource           string    `json:"installed_version_source,omitempty"`
	VersionObservedAt       time.Time `json:"installed_version_observed_at,omitzero"`
	VersionKind             string    `json:"installed_version_kind,omitempty"`
	VersionState            string    `json:"installed_version_state"`
	VersionSupport          string    `json:"installed_version_support"`
	// VersionSupportReason is set when VersionSupport is unverified. Verified
	// captures report the harness's own version (Codex cli_version, Claude
	// Code record version, Cursor hook cursor_version) in HarnessVersions; the
	// installed version comes from discovery and may be numbered differently.
	VersionSupportReason    string              `json:"installed_version_support_reason,omitempty"`
	Capabilities            captureCapabilities `json:"capabilities"`
	verifiedHarnessVersions []string
	Projects                []projectCaptureStatus `json:"projects"`

	Code  string `json:"code"`
	Hooks string `json:"hooks"`
	// OtherInstallations lists the data directories of other agent-archive
	// installations whose hooks are in this app's hook file (or, for one
	// whose directory cannot be read, its command). This installation never
	// changes them.
	OtherInstallations []string `json:"other_installations,omitempty"`
	Name               string   `json:"name"`
	//lint:ignore LV1001 an open-ended, human-readable label built from many phrasings; statusCode maps it to the stable Code
	State           string    `json:"state"`
	Sessions        int       `json:"sessions"`
	LastPublishedAt time.Time `json:"last_published_at,omitzero"`
}

type projectCaptureStatus struct {
	ProjectID         string    `json:"project_id"`
	ProjectRoot       string    `json:"project_root"`
	ActivatedAt       time.Time `json:"activated_at"`
	Configured        bool      `json:"configured"`
	HookObserved      bool      `json:"hook_observed"`
	CapturedLocally   bool      `json:"captured_locally"`
	Published         bool      `json:"published"`
	ReadBackVerified  bool      `json:"read_back_verified"`
	PublishedSessions int       `json:"published_sessions"`
	VerifiedSessions  int       `json:"verified_sessions"`
	VerificationState string    `json:"verification_state"`
	VerifiedAt        time.Time `json:"verified_at,omitzero"`
}

type statusView struct {
	PrivacyEvidence storage.PrivacyReport `json:"privacy_evidence"`
	ConfigurationID string                `json:"configuration_id,omitempty"`
	Authentication  storageHealth         `json:"authentication"`

	Code    string `json:"code"`
	Version int    `json:"schema_version"`
	//lint:ignore LV1001 an open-ended, human-readable label built from many phrasings; statusCode maps it to the stable Code
	State   string `json:"state"`
	Storage string `json:"storage,omitempty"`
	// StorageVerifiedAt is when setup's storage check (write, read, list,
	// and delete of a probe object) last passed for this configuration.
	StorageVerifiedAt time.Time `json:"storage_verified_at,omitzero"`
	// StorageAccessConfirmedAt is the latest confirmation that this
	// destination is reachable with the configured credentials: setup's
	// check, or the collector's last verified storage health for the same
	// configuration (its access probe or a pass that uploaded).
	// StorageAccessConfirmedBy names which: "setup" or "collector".
	StorageAccessConfirmedAt time.Time              `json:"storage_access_confirmed_at,omitzero"`
	StorageAccessConfirmedBy storageAccessConfirmer `json:"storage_access_confirmed_by,omitempty"`
	Privacy                  string                 `json:"privacy"`
	Background               string                 `json:"background"`
	Paused                   bool                   `json:"paused"`
	Projects                 []string               `json:"projects"`
	Apps                     []appStatus            `json:"applications"`
	Collector                state.Status           `json:"collector"`
	CaptureDiagnostics       []capture.Diagnostic   `json:"capture_diagnostics,omitempty"`
	// ImportedSessions counts sessions `agent-archive backfill` registered,
	// not their subagents; ImportedPending counts those the collector still
	// has to upload, and ImportedWithIssues those with a capture gap or a
	// failed last scan. LastImport names the most recent import batch.
	ImportedSessions   int    `json:"imported_sessions"`
	ImportedPending    int    `json:"imported_pending"`
	ImportedWithIssues int    `json:"imported_with_issues"`
	LastImport         string `json:"last_import"`
	// Warnings lists advisory local files that could not be read; status
	// still reports everything else.
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next_action"`
	// configured is whether a configuration exists; the text status shows
	// only the state and next step without one.
	configured bool
}

func runStatusCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("status", stderr)
	jsonOut := fs.Bool("json", false, "print a versioned JSON document")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	view, err := readStatus(env)
	if err != nil {
		terminal.Printf(stderr, "Cannot read archive status: %v\n", err)
		return 1
	}
	if *jsonOut {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(view); err != nil {
			terminal.Println(stderr, err)
			return 1
		}
		// Errors and names in the view can come from the bucket.
		terminal.Print(stdout, string(archive.DisplayJSON(encoded.Bytes())))
		return 0
	}
	terminal.Printf(stdout, "Agent Archive — %s\n\n", view.State)
	if !view.configured {
		// Nothing is installed to report on: no storage, collector, or apps.
		terminal.Printf(stdout, "Next: %s\n", view.Next)
		return 0
	}
	if view.Storage != "" {
		terminal.Printf(stdout, "Storage:       %s\nAccess:        %s\n", view.Storage, storageAccessLine(view))
		printBucketPrivacy(stdout, view.PrivacyEvidence)
	}
	checked := "not checked yet"
	if !view.Authentication.CheckedAt.IsZero() {
		checked = "checked " + formatTimeOrNever(view.Authentication.CheckedAt)
	}
	if view.Authentication.Context != "" {
		checked += "; " + view.Authentication.Context
	}
	terminal.Printf(stdout, "Authentication: %s (%s)\n", view.Authentication.State, checked)
	terminal.Printf(stdout, "Background:    %s\n", view.Background)
	if view.Paused {
		terminal.Println(stdout, "Collection:    paused")
	}
	terminal.Printf(stdout, "Projects:      %d included\nPending:       %d session(s)\nLast scan:     %s\nLast publish:  %s\n", len(view.Projects), view.Collector.PendingCount, formatTimeOrNever(view.Collector.LastScanAt), formatTimeOrNever(view.Collector.LastPublishedAt))
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("%d session(s), %d waiting to upload", view.ImportedSessions, view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		terminal.Printf(stdout, "Imported:      %s\n", imported)
	}
	for _, app := range view.Apps {
		gaps := ""
		if len(app.CaptureGaps) > 0 {
			gaps = fmt.Sprintf("; %d with a capture gap", app.SessionsWithCaptureGaps)
		}
		terminal.Printf(stdout, "%s: %s (%d session(s)%s); hooks %s\n", appName(app.Name), app.State, app.Sessions, gaps, app.Hooks)
		if app.Trust == "unknown" {
			terminal.Println(stdout, "  Hook trust: unknown here; it is granted inside the app and is not observable from this Mac's files.")
		}
		terminal.Printf(stdout, "  Installed version: %s; support %s%s.\n", installedVersionLabel(app), app.VersionSupport, versionSupportNote(app))
		if app.Capabilities.FreshStart.State == capabilityUnavailable {
			terminal.Printf(stdout, "  Fresh-start capture: unavailable. %s\n", app.Capabilities.FreshStart.NextAction)
		}
		for _, pair := range app.Projects {
			terminal.Printf(stdout, "  Project %s: %s.\n", pair.ProjectRoot, pair.VerificationState)
		}
		switch {
		case app.ReadBackVerified && !app.VerifiedAt.IsZero():
			terminal.Printf(stdout, "  Read-back verified: %s; evidence is for that publication.\n", formatTimeOrNever(app.VerifiedAt))
		case !app.VerifiedAt.IsZero():
			// Some evidence exists but not every project (or session) is
			// covered, so do not call the app verified on the line below
			// its "read-back pending" state.
			terminal.Printf(stdout, "  Last read-back: %s (%s).\n", formatTimeOrNever(app.VerifiedAt), readBackProgress(app))
		}
		if app.VerificationDetail != "" {
			terminal.Printf(stdout, "  Read-back: %s\n", app.VerificationDetail)
		}
		if len(app.CaptureGaps) > 0 {
			terminal.Printf(stdout, "  Capture gaps: %d recorded across %d session(s); see status --json for details.\n", len(app.CaptureGaps), app.SessionsWithCaptureGaps)
		}
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		terminal.Printf(stdout, "Capture skipped in %s (%s): %s at %s.\n", diagnostic.ProjectRoot, appName(diagnostic.Harness), capture.DiagnosticMessage(diagnostic.Code), formatTimeOrNever(diagnostic.ObservedAt))
	}
	if view.Collector.LastError != "" {
		terminal.Printf(stdout, "Last error:    %s\n", view.Collector.LastError)
	}
	if n := len(view.Collector.QuarantinedFiles); n > 0 {
		terminal.Printf(stdout, "Quarantined:   %d local state file(s) could not be read and were moved aside; their sessions keep their other evidence. See status --json for the files, then delete them.\n", n)
	}
	if n := view.Collector.UnrefreshableSummaries; n > 0 {
		terminal.Printf(stdout, "Summaries:     %d session summary(ies) cannot be refreshed by this version and stay as published until the session changes.\n", n)
	}
	for _, warning := range view.Warnings {
		terminal.Printf(stdout, "Warning:       %s\n", warning)
	}
	terminal.Printf(stdout, "\nNext: %s\n", view.Next)
	return 0
}

// storageAccessConfirmer is what last confirmed access to the destination,
// as status --json reports it in storage_access_confirmed_by.
type storageAccessConfirmer string

const (
	storageAccessConfirmedBySetup     storageAccessConfirmer = "setup"
	storageAccessConfirmedByCollector storageAccessConfirmer = "collector"
)

// storageAccessLine is the text status's Access line: when access to the
// destination was last confirmed, and by what.
func storageAccessLine(view statusView) string {
	switch view.StorageAccessConfirmedBy {
	case storageAccessConfirmedBySetup:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by setup's storage check (write, read, list, delete)"
	case storageAccessConfirmedByCollector:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by the collector's last successful storage access"
	}
	return "not confirmed yet"
}

// storageLabel names the destination as "provider / bucket / prefix",
// leaving out an empty prefix rather than ending in a bare separator.
func storageLabel(cfg credentials.Config) string {
	parts := []string{cfg.Provider, cfg.Bucket}
	if cfg.Prefix != "" {
		parts = append(parts, cfg.Prefix)
	}
	return strings.Join(parts, " / ")
}

// blockedReasonDetail explains a recorded capture gap. A missing transcript is
// the ordinary end of an archived session's local life, not a defect, and it
// is the one reason that can end on its own, so it does not borrow the
// permanent wording the other reasons need.
func blockedReasonDetail(reason state.BlockedReason) string {
	switch reason {
	case state.BlockedReasonTranscriptMissing:
		return "The application has deleted its own transcript, as each one does on its own schedule. The last published snapshot stays retained and readable, and capture resumes by itself if the file returns."
	case state.BlockedReasonRecordTooLarge:
		return fmt.Sprintf("One record in the transcript (or a plain-text transcript as a whole) is larger than the %d MiB record size limit, so the transcript cannot be read. The last published snapshot, if any, stays retained, and capture resumes when the transcript changes.", archive.MaxRecordBytes>>20)
	case state.BlockedReasonTranscriptRewritten, state.BlockedReasonTranscriptTooLarge:
		// Permanent for the current transcript: the general wording below.
	}
	return "The current transcript can no longer be captured; the last published snapshot, if any, stays retained."
}

// versionSupportNote explains an unverified installed version in the text
// status without changing the support state or reason code.
func versionSupportNote(app appStatus) string {
	//lint:ignore LV1001 the reason codes are untyped constants in capabilities.go, which computes this field
	switch app.VersionSupportReason {
	case supportReasonNoVerifiedCapture:
		return " (no session from this version has been published and read back yet)"
	case supportReasonNoMatchingVersion:
		return " (verified sessions came from a different version)"
	case supportReasonVersionSourceMismatch:
		return " (installed version and captured versions use different numbering; cannot be compared)"
	}
	return ""
}

// readBackProgress summarises how much of an app's evidence is verified:
// projects when the configuration has any, otherwise published sessions.
func readBackProgress(app appStatus) string {
	if len(app.Projects) == 0 {
		return fmt.Sprintf("%d of %d sessions verified", app.VerifiedSessions, app.PublishedSessions)
	}
	verified := 0
	for _, pair := range app.Projects {
		if pair.ReadBackVerified {
			verified++
		}
	}
	return fmt.Sprintf("%d of %d projects verified", verified, len(app.Projects))
}

func installedVersionLabel(app appStatus) string {
	if app.InstalledVersion != "" {
		return app.InstalledVersion
	}
	if app.VersionState != "" {
		return app.VersionState
	}
	return "unknown"
}

func readStatus(env Env) (view statusView, err error) {
	defer func() {
		view.Code = statusCode(view.State)
		for i := range view.Apps {
			view.Apps[i].Code = statusCode(view.Apps[i].State)
		}
	}()
	// Before setup there is no collector or storage to ask about: the job is
	// missing, as launchd reports a job that is not loaded, and storage not
	// configured; neither is unknown.
	view = statusView{
		Version:        3,
		State:          "Not set up",
		Privacy:        "not_verified",
		Background:     "missing",
		Authentication: storageHealth{State: "not_configured"},
		Projects:       []string{},
		Apps:           []appStatus{},
		Next:           "Run agent-archive setup to get started.",
	}
	home, err := env.readHome()
	if err != nil {
		return view, err
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return view, err
	}
	if _, err := os.Stat(draftPath(home)); err == nil {
		view.State = "Setup saved"
		view.Next = "Run agent-archive setup to continue your saved choices."
		if _, _, problem, _ := readDraft(home); problem != "" {
			view.Next = fmt.Sprintf("The saved setup in %s cannot be used (%s). Run agent-archive setup: it offers to move it aside and start again.", draftPath(home), problem)
			view.Warnings = append(view.Warnings, view.Next)
		}
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
	if !found {
		return view, nil
	}
	view.configured = true
	view.Background = "unknown"
	view.Storage = storageLabel(cfg.Storage)
	view.StorageVerifiedAt = cfg.StorageVerifiedAt
	view.PrivacyEvidence = currentBucketPrivacy(cfg, env.now())
	view.Privacy = view.PrivacyEvidence.State
	view.ConfigurationID = configurationID(cfg)
	// Advisory files: one that cannot be read is left out with a warning,
	// never a reason to report nothing at all.
	view.CaptureDiagnostics, err = capture.ReadDiagnostics(home)
	if err != nil {
		view.Warnings = append(view.Warnings, unreadableWarning(capture.DiagnosticsPath(home), err, "The next capture diagnostic replaces it; deleting it loses only past diagnostics."))
		view.CaptureDiagnostics = nil
	}
	view.CaptureDiagnostics = capture.IncludedDiagnostics(view.CaptureDiagnostics, cfg.Archive.Projects)
	view.Authentication.State = "unknown"
	if err := local.Read(filepath.Join(home, "storage-health.json"), &view.Authentication); err != nil && !os.IsNotExist(err) {
		view.Warnings = append(view.Warnings, unreadableWarning(filepath.Join(home, "storage-health.json"), err, "The background collector checks storage again and replaces it within a few minutes."))
		view.Authentication = storageHealth{State: "unknown"}
	}
	if view.Authentication.ConfigurationID != "" && view.Authentication.ConfigurationID != view.ConfigurationID {
		view.Authentication.State = "stale_configuration"
	}
	// Setup records when its storage check passed; afterwards the collector
	// confirms the same destination again (its access probe, or a pass that
	// uploaded). Report the latest, so the Access line never says "not
	// confirmed" beside an Authentication line that says verified. Health
	// without this configuration's ID is not evidence for it.
	if !view.StorageVerifiedAt.IsZero() {
		view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy = view.StorageVerifiedAt, storageAccessConfirmedBySetup
	}
	if view.Authentication.State == "verified" && view.Authentication.ConfigurationID == view.ConfigurationID && view.Authentication.CheckedAt.After(view.StorageAccessConfirmedAt) {
		view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy = view.Authentication.CheckedAt, storageAccessConfirmedByCollector
	}
	// The background probe only runs while collection is active, so a paused
	// install keeps its last known state (with its checked time) rather than
	// being called stale for a check that was deliberately not repeated.
	if !cfg.Paused && view.Authentication.State == "verified" && env.now().Sub(view.Authentication.CheckedAt) > storageHealthStaleAfter {
		view.Authentication.State = "stale"
	}
	view.Paused = cfg.Paused
	for _, p := range cfg.Archive.Projects {
		if p.Included {
			view.Projects = append(view.Projects, p.Root)
		}
	}
	store := state.OpenReadOnly(home)
	view.Collector, err = store.LoadStatus()
	if err != nil {
		view.Warnings = append(view.Warnings, unreadableWarning(filepath.Join(home, "status.json"), err, "The collector replaces it on its next pass; agent-archive sync runs one now."))
		view.Collector = state.Status{}
	}
	regs, reqs, stateWarnings := statusState(home, store)
	view.Warnings = append(view.Warnings, stateWarnings...)
	// A session whose own files cannot be read is left out of every count
	// below, with one warning naming it.
	skipped := map[string]bool{}
	skip := func(id string, err error) {
		if !skipped[id] {
			skipped[id] = true
			view.Warnings = append(view.Warnings, fmt.Sprintf("Local state of session %s could not be read (%v); status left that session out.", id, err))
		}
	}
	queued := state.QueuedRequests(reqs)
	// What each session still owes, read once: the pending count and the
	// import counts below both come from it (state.Outstanding).
	owed := map[string]state.Outstanding{}
	pending := 0
	for _, reg := range regs {
		accepted := cfg.AcceptSession(reg)
		if !accepted && !reg.Imported() {
			continue
		}
		o, err := store.Outstanding(reg, queued[reg.ArchiveSessionID])
		if err != nil {
			skip(reg.ArchiveSessionID, err)
			continue
		}
		owed[reg.ArchiveSessionID] = o
		if accepted && o.Pending() {
			pending++
		}
	}
	if pending > view.Collector.PendingCount {
		view.Collector.PendingCount = pending
	}
	var readable []archive.SessionRegistration
	for _, reg := range regs {
		if !skipped[reg.ArchiveSessionID] {
			readable = append(readable, reg)
		}
	}
	regs = readable
	view.ImportedSessions, view.ImportedPending, view.ImportedWithIssues = importedSessionCounts(cfg, regs, owed, view.Collector.SessionIssues)
	// One unreadable import file must not hide the rest of status.
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		view.Warnings = append(view.Warnings, err.Error())
	}
	if len(batches) > 0 {
		view.LastImport = batches[len(batches)-1].ID
	}
	for _, name := range cfg.Harnesses {
		app := appStatus{Name: name, State: "waiting for first session", Configured: true, Trust: "unknown", VerificationState: "not_verified"}
		pairIndex := map[string]int{}
		for _, project := range cfg.Archive.Projects {
			if !project.Included {
				continue
			}
			if _, dup := pairIndex[project.Root]; dup {
				// A hand-edited configuration can repeat a root. The first
				// entry owns the pair; a second would never receive evidence
				// and would pin the app unverified.
				continue
			}
			pairIndex[project.Root] = len(app.Projects)
			app.Projects = append(app.Projects, projectCaptureStatus{
				ProjectID: string(project.ProjectID), ProjectRoot: project.Root,
				ActivatedAt: project.ActivatedAt, Configured: true, VerificationState: "not_verified",
			})
		}
		var readBackIssue verificationOutcome
		for _, reg := range regs {
			// An import is not evidence that this app's hooks work: it
			// never counts toward the app's sessions, hook observation,
			// or verification. The Imported line reports it instead.
			if reg.Imported() || reg.Harness.Name != name || !cfg.AcceptSession(reg) {
				continue
			}
			// AcceptSession only admits a root without a configured pair
			// under its legacy branch (no projects configured at all). The
			// collector still publishes such sessions, so they count toward
			// the app even though there is no pair to attribute them to.
			var pair *projectCaptureStatus
			if position, found := pairIndex[reg.ProjectRoot]; found {
				pair = &app.Projects[position]
			}
			app.Sessions++
			app.HookObserved = true
			gapsBefore := len(app.CaptureGaps)
			if pair != nil {
				pair.HookObserved = true
			}
			if issue := view.Collector.SessionIssues[reg.ArchiveSessionID]; issue != "" {
				app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: issue, Detail: "Last scan could not update this session; retained evidence was kept. Run agent-archive sync for the failure."})
			}
			if reg.Harness.Version != "" && !containsString(app.HarnessVersions, reg.Harness.Version) {
				app.HarnessVersions = append(app.HarnessVersions, reg.Harness.Version)
			}
			if app.State == "waiting for first session" {
				app.State = "hook observed; waiting for capture"
			}
			bundle, _, cacheStatus, found, err := store.LoadPublished(reg.ArchiveSessionID)
			if err != nil {
				skip(reg.ArchiveSessionID, err)
				continue
			}
			if cacheStatus == state.CacheStatusBlocked {
				reason, _, e := store.LoadBlocked(reg.ArchiveSessionID)
				if e != nil {
					skip(reg.ArchiveSessionID, e)
					continue
				}
				app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: string(reason), Detail: blockedReasonDetail(reason)})
			}
			if found {
				if cacheStatus != state.CacheStatusBlocked {
					app.CapturedLocally = true
					if pair != nil {
						pair.CapturedLocally = true
					}
				}
				app.CaptureGaps = append(app.CaptureGaps, bundle.Capture.Gaps...)
				if version := bundle.Capture.AdapterVersion; version != "" && !containsString(app.AdapterVersions, version) {
					app.AdapterVersions = append(app.AdapterVersions, version)
				}
			}
			// A blocked session with no publication has captured nothing;
			// one that was published earlier still counts as published below.
			if found && cacheStatus != state.CacheStatusBlocked && app.LastPublishedAt.IsZero() {
				app.State = "captured locally"
			}
			if len(app.CaptureGaps) > gapsBefore {
				app.SessionsWithCaptureGaps++
			}
			publishedBundle, at, published, e := store.LoadLastPublished(reg.ArchiveSessionID)
			if e != nil {
				skip(reg.ArchiveSessionID, e)
				continue
			}
			if published {
				app.Published = true
				app.PublishedSessions++
				if pair != nil {
					pair.Published = true
					pair.PublishedSessions++
				}
				app.State = "published; read-back pending"
				verification, e := readVerification(home, reg.ArchiveSessionID)
				if e != nil {
					skip(reg.ArchiveSessionID, e)
					continue
				}
				verificationConfigurationID := sessionVerificationConfigurationID(cfg, reg)
				if verification.ConfigurationID == verificationConfigurationID && verification.PublishedAt.Equal(at) && !verification.VerifiedAt.IsZero() {
					app.VerifiedSessions++
					if pair != nil {
						pair.VerifiedSessions++
						if verification.VerifiedAt.After(pair.VerifiedAt) {
							pair.VerifiedAt = verification.VerifiedAt
						}
					}
					app.VerificationState = "verified_at_recorded_time"
					if verification.VerifiedAt.After(app.VerifiedAt) {
						app.VerifiedAt = verification.VerifiedAt
					}
					app.State = "published; source verified"
					if version := publishedBundle.Capture.Harness.Version; version != "" && !containsString(app.verifiedHarnessVersions, version) {
						app.verifiedHarnessVersions = append(app.verifiedHarnessVersions, version)
					}
				} else if !verification.VerifiedAt.IsZero() {
					app.VerificationState = "stale"
				} else if verification.ConfigurationID == verificationConfigurationID && verification.PublishedAt.Equal(at) && verification.Attempts > 0 {
					// The collector tried and could not read this publication
					// back; a mismatch outranks a transient failure.
					if verification.Outcome == verificationOutcomeMismatch || readBackIssue != verificationOutcomeMismatch {
						readBackIssue = verification.Outcome
						app.VerificationDetail = fmt.Sprintf("%s: %s (attempt %d; next retry %s)", verification.Outcome, verification.LastError, verification.Attempts, formatTimeOrNever(verification.NextRetryAt))
					}
				}
				if at.After(app.LastPublishedAt) {
					app.LastPublishedAt = at
				}
			}
		}
		app.ReadBackVerified = len(app.Projects) > 0
		if len(app.Projects) == 0 {
			// Nothing to require per pair (legacy configuration without
			// projects): the sessions themselves are the evidence.
			app.ReadBackVerified = app.PublishedSessions > 0 && app.VerifiedSessions == app.PublishedSessions
		}
		for i := range app.Projects {
			pair := &app.Projects[i]
			pair.ReadBackVerified = pair.PublishedSessions > 0 && pair.VerifiedSessions == pair.PublishedSessions
			switch {
			case pair.ReadBackVerified:
				pair.VerificationState = "verified_at_recorded_time"
			case pair.Published:
				pair.VerificationState = "incomplete"
			case pair.CapturedLocally:
				pair.VerificationState = "captured_local"
			case pair.HookObserved:
				pair.VerificationState = "hook_observed"
			default:
				pair.VerificationState = "not_verified"
			}
			if !pair.ReadBackVerified {
				app.ReadBackVerified = false
			}
		}
		if app.Published && !app.ReadBackVerified {
			app.State = "published; read-back pending"
			app.VerificationState = "incomplete"
			switch readBackIssue {
			case verificationOutcomeMismatch:
				app.VerificationState = "read_back_mismatch"
			case verificationOutcomeFailed:
				app.VerificationState = "read_back_failed"
			case verificationOutcomeVerified:
				// A verified session is not a read-back issue.
			}
		}
		view.Apps = append(view.Apps, app)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return view, err
	}
	// Hooks are checked against the path setup installed, not the path this
	// status process runs from; older configurations fall back to the latter.
	executable, executableErr := cfg.InstalledExecutable, error(nil)
	if executable == "" {
		executable, executableErr = env.executable()
	}
	// Every installed hook runs the executable setup recorded. If that file
	// was moved or deleted, the hook configuration still matches exactly, so
	// comparing it alone would report healthy hooks that fail on every event.
	// An uninstalled archive has no hooks left to break.
	hookFiles := env.installedHookFiles(userHome, cfg)
	binaryProblem := ""
	if cfg.InstalledExecutable != "" && cfg.Archive.Enabled {
		binaryProblem = executableProblem(cfg.InstalledExecutable)
	}
	if binaryProblem != "" {
		view.Warnings = append(view.Warnings, fmt.Sprintf("The agent-archive executable that setup installed at %s is %s; every app hook runs it, so capture has stopped.", cfg.InstalledExecutable, binaryProblem))
	}
	discovered, err := readApplicationDiscoveries(home)
	if err != nil {
		// Advisory only: a damaged observation file degrades installed
		// versions to unknown rather than hiding the rest of the status.
		view.Warnings = append(view.Warnings, fmt.Sprintf("Installed versions could not be read from %s: %v. Run agent-archive setup to refresh them.", applicationDiscoveriesPath(home), err))
		discovered = map[string]applicationDiscovery{}
	}
	for i := range view.Apps {
		appDiscovery := discovered[view.Apps[i].Name]
		if appDiscovery.VersionState == "" {
			appDiscovery.VersionState = "unknown"
		}
		if !appDiscovery.ObservedAt.IsZero() && (env.now().Before(appDiscovery.ObservedAt) || env.now().Sub(appDiscovery.ObservedAt) > 24*time.Hour) {
			appDiscovery.VersionState = "stale"
		}
		view.Apps[i].Installed = appDiscovery.Installed
		view.Apps[i].InstalledVersion = appDiscovery.Version
		view.Apps[i].VersionSource = appDiscovery.VersionSource
		view.Apps[i].VersionObservedAt = appDiscovery.ObservedAt
		view.Apps[i].VersionKind = appDiscovery.VersionKind
		view.Apps[i].VersionState = appDiscovery.VersionState
		view.Apps[i].Capabilities = captureCapabilityProfile(view.Apps[i].Name)
		view.Apps[i].VersionSupport, view.Apps[i].VersionSupportReason = installedVersionSupportDetail(appDiscovery, view.Apps[i].verifiedHarnessVersions)
		in := env.installation(home, userHome)
		installed, e := hooks.Installed(hookFiles, in.hook(executable), view.Apps[i].Name)
		if others, err := hooks.OtherInstallations(hookFiles, in.owner(), view.Apps[i].Name); err == nil && len(others) > 0 {
			for _, other := range others {
				view.Apps[i].OtherInstallations = append(view.Apps[i].OtherInstallations, cmp.Or(other.DataHome, other.Command, "default"))
			}
			view.Warnings = append(view.Warnings, describeOtherInstallations(hookFiles[view.Apps[i].Name], view.Apps[i].Name, others))
		}
		switch {
		case binaryProblem != "":
			view.Apps[i].Hooks = hooksBroken
		case e != nil:
			view.Apps[i].Hooks = "unknown"
			view.Warnings = append(view.Warnings, fmt.Sprintf("%s hooks could not be checked: %v. Fix or restore %s, then run agent-archive setup.", appName(view.Apps[i].Name), e, hookFiles[view.Apps[i].Name]))
		case executableErr != nil:
			view.Apps[i].Hooks = "unknown"
		case !installed:
			view.Apps[i].Hooks = "missing or incomplete"
		default:
			view.Apps[i].Hooks = "installed"
		}
	}
	plist := env.installation(home, userHome).installedCollectorPlist()
	view.Background = env.jobState(plist)
	// launchd reports a job whose program is gone as loaded (it only fails
	// when it fires), so read the program the LaunchAgent actually runs.
	backgroundProgram, backgroundProblem := "", ""
	if cfg.Archive.Enabled {
		if data, err := os.ReadFile(plist); err == nil {
			if program, err := hooks.LaunchAgentProgram(data); err == nil {
				backgroundProgram, backgroundProblem = program, executableProblem(program)
			}
		}
	}
	if backgroundProblem != "" {
		view.Background = backgroundBroken
		view.Warnings = append(view.Warnings, fmt.Sprintf("The background collector's LaunchAgent runs %s, which is %s, so scheduled collection has stopped.", backgroundProgram, backgroundProblem))
	}
	view.State = "Ready"
	view.Next = "Keep working. Run agent-archive list to inspect archived sessions."
	if len(view.Apps) == 0 {
		view.State = "Needs attention"
		view.Next = "Run agent-archive setup and select at least one application."
	} else if len(view.Projects) == 0 {
		view.State = "Needs attention"
		view.Next = "Run agent-archive setup and include at least one project."
	}
	for _, app := range view.Apps {
		for _, pair := range app.Projects {
			if pair.ReadBackVerified {
				continue
			}
			view.State = "Waiting for capture"
			switch {
			case app.Capabilities.FreshStart.State == capabilityUnavailable && !pair.HookObserved:
				view.Next = app.Capabilities.FreshStart.NextAction
			case pair.Published:
				view.Next = "Run agent-archive sync to retry read-back verification for " + appName(app.Name) + " in " + pair.ProjectRoot + "."
			case pair.HookObserved:
				view.Next = "Run agent-archive sync to capture and publish the " + appName(app.Name) + " session in " + pair.ProjectRoot + "."
			default:
				view.Next = "Review hook approval in " + appName(app.Name) + ", then start a new session in " + pair.ProjectRoot + "."
			}
			break
		}
		if view.State == "Waiting for capture" {
			break
		}
	}
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			view.State = "Needs attention"
			view.Next = "Run agent-archive setup to check the hooks for " + appName(app.Name) + "."
			if len(app.OtherInstallations) > 0 {
				// setup refuses to install beside them, so it is not the way out.
				view.Next = "Another agent-archive installation's hooks are in " + appName(app.Name) + "'s hook file (see the warning above). Remove that installation, or give this one its own HOME, then run agent-archive setup."
			}
			break
		}
	}
	if !setupjournal.JobActive(view.Background) {
		view.State = "Needs attention"
		view.Next = "Run agent-archive setup to restore the background collector."
		if view.Background == setupjournal.JobAnotherInstallation {
			// setup refuses to replace that job, so it is not the way out.
			view.Next = fmt.Sprintf("Another agent-archive installation's collector runs under this installation's launchd label (%s), and setup will not replace it. Set AGENT_ARCHIVE_HOME to a data directory of this installation's own, or uninstall the other installation.", launchLabel(plist))
		}
	}
	if !view.Collector.LastScanAt.IsZero() && env.now().Sub(view.Collector.LastScanAt) > 5*time.Minute {
		view.State = "Needs attention"
		view.Next = "The last scan is over 5 minutes old. Run agent-archive sync to check collection."
	}
	// sync cannot help while another process holds the collector lock. The
	// holder records when it took the lock, so a pass that started a moment
	// ago is never mistaken for a hung one.
	if record, ok := readCollectorLockRecord(home); ok && env.now().Sub(record.Since) > collectLockStuckAfter && processAlive(record.PID) && collectorLockHeld(home) {
		view.State = "Needs attention"
		view.Next = fmt.Sprintf("Collection is stuck: %s (process %d) has held the collector lock since %s, %s, well past a pass's time limit. If that command is no longer doing anything, quit process %d (in Activity Monitor or with kill %d); the next pass then resumes.", record.Holder, record.PID, record.Since.UTC().Format("2006-01-02 15:04 UTC"), durationAgo(env.now().Sub(record.Since)), record.PID, record.PID)
	}
	if view.Collector.LastError != "" {
		view.State = "Needs attention"
		view.Next = "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage."
		// A Keychain failure has one specific fix; status.json keeps only
		// the error text, so it is recognized from that.
		if action := credentials.RecoveryActionForMessage(view.Collector.LastError); action != "" {
			view.Next = action
		}
	}
	if cfg.Paused {
		view.State = "Paused"
		view.Next = "Run agent-archive resume when ready. Registered sessions can catch up after resume."
	}
	// A moved or deleted binary is the root cause of every symptom above (a
	// stale scan, a failing hook), and pausing does not stop the apps from
	// running hooks that now fail, so it outranks all of them.
	if binaryProblem != "" || backgroundProblem != "" {
		moved := cfg.InstalledExecutable
		if binaryProblem == "" {
			moved = backgroundProgram
		}
		view.State = "Needs attention"
		view.Next = fmt.Sprintf("agent-archive is no longer usable at %s. Run agent-archive setup from the binary's new location to point the hooks and background collector at it.", moved)
	}
	if !cfg.Archive.Enabled {
		view.State = "Not installed"
		view.Next = "Local data is kept. Run agent-archive setup to reinstall."
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
	return view, nil
}

// collectorLockHeld reports whether another process holds collector.lock
// right now. flock has no query, so it tries the lock and releases it at
// once; a collector starting in that instant skips one pass, as it would
// for any other holder.
func collectorLockHeld(home string) bool {
	unlock, err := local.Lock(home)
	if err != nil {
		return errors.Is(err, local.ErrBusy)
	}
	unlock()
	return false
}

// importedSessionCounts counts the sessions backfill imported, leaving out
// their subagents, and how many of those are pending (state.Outstanding)
// under cfg, the same definition as status's own pending count. withIssues
// counts those with a recorded capture gap or a failed last scan: status
// leaves imports out of each app's own gaps and issues, so they are
// reported here. owed holds every import's Outstanding.
func importedSessionCounts(cfg config.Config, regs []archive.SessionRegistration, owed map[string]state.Outstanding, issues map[string]string) (imported, pending, withIssues int) {
	for _, reg := range regs {
		if !reg.Imported() || reg.ParentSessionID != "" {
			continue
		}
		imported++
		o := owed[reg.ArchiveSessionID]
		if o.Blocked || issues[reg.ArchiveSessionID] != "" {
			withIssues++
		}
		if cfg.AcceptSession(reg) && o.Pending() {
			pending++
		}
	}
	return imported, pending, withIssues
}

// importPending reports whether an imported session is pending under cfg:
// the one definition state.Outstanding gives, for a caller that has not
// listed the queued requests.
func importPending(store *state.Store, cfg config.Config, reg archive.SessionRegistration) (bool, error) {
	if !cfg.AcceptSession(reg) {
		return false, nil
	}
	_, requested, err := store.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	owed, err := store.Outstanding(reg, requested)
	if err != nil {
		return false, err
	}
	return owed.Pending(), nil
}

const (
	// hooksBroken: the hook configuration is in place but runs an executable
	// that no longer exists or cannot be run.
	hooksBroken = "broken"
	// backgroundBroken: the LaunchAgent is in place but runs such an executable.
	backgroundBroken = "broken"
)

// executableProblem says why path cannot be run, or "" when it can.
func executableProblem(path string) string {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "missing"
	case err != nil:
		return "unreadable"
	case !info.Mode().IsRegular():
		return "not a regular file"
	case info.Mode().Perm()&0o111 == 0:
		return "not executable"
	}
	return ""
}

func formatTimeOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// Codes are an API; changing display wording must not change them.
func statusCode(label string) string {
	codes := map[string]string{
		"Not set up": "not_configured", "Setup saved": "setup_pending",
		"Ready": "ready", "Waiting for capture": "awaiting_capture",
		"Needs attention": "needs_attention", "Paused": "paused",
		"Not installed": "not_installed", "Setup needs recovery": "recovery_required",
		"waiting for first session":          "awaiting_session",
		"hook observed; waiting for capture": "hook_observed",
		"published; read-back pending":       "published",
		"captured locally":                   "captured_local", "published; source verified": "published_source_verified",
	}
	if code, ok := codes[label]; ok {
		return code
	}
	return "unknown"
}
