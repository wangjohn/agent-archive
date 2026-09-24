package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type appStatus struct {
	PublishedSessions int       `json:"published_sessions"`
	VerifiedSessions  int       `json:"verified_sessions"`
	Configured        bool      `json:"configured"`
	HookObserved      bool      `json:"hook_observed"`
	CapturedLocally   bool      `json:"captured_locally"`
	Published         bool      `json:"published"`
	ReadBackVerified  bool      `json:"read_back_verified"`
	VerifiedAt        time.Time `json:"verified_at,omitempty"`
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
	CaptureGaps       []archive.CaptureGap `json:"capture_gaps,omitempty"`
	Installed         bool                 `json:"installed"`
	InstalledVersion  string               `json:"installed_version,omitempty"`
	VersionSource     string               `json:"installed_version_source,omitempty"`
	VersionObservedAt time.Time            `json:"installed_version_observed_at,omitempty"`
	VersionKind       string               `json:"installed_version_kind,omitempty"`
	VersionState      string               `json:"installed_version_state"`
	VersionSupport    string               `json:"installed_version_support"`
	// VersionSupportReason is set when VersionSupport is unverified. Verified
	// captures report the harness's own version (Codex cli_version, Claude
	// Code record version, Cursor hook cursor_version) in HarnessVersions; the
	// installed version comes from discovery and may be numbered differently.
	VersionSupportReason    string              `json:"installed_version_support_reason,omitempty"`
	Capabilities            captureCapabilities `json:"capabilities"`
	verifiedHarnessVersions []string
	Projects                []projectCaptureStatus `json:"projects"`

	Code            string    `json:"code"`
	Hooks           string    `json:"hooks"`
	Name            string    `json:"name"`
	State           string    `json:"state"`
	Sessions        int       `json:"sessions"`
	LastPublishedAt time.Time `json:"last_published_at,omitempty"`
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
	VerifiedAt        time.Time `json:"verified_at,omitempty"`
}
type statusView struct {
	PrivacyEvidence storage.PrivacyReport `json:"privacy_evidence"`
	ConfigurationID string                `json:"configuration_id,omitempty"`
	Authentication  storageHealth         `json:"authentication"`

	Code               string              `json:"code"`
	Version            int                 `json:"schema_version"`
	State              string              `json:"state"`
	Storage            string              `json:"storage,omitempty"`
	StorageVerifiedAt  time.Time           `json:"storage_verified_at,omitempty"`
	Privacy            string              `json:"privacy"`
	Background         string              `json:"background"`
	Paused             bool                `json:"paused"`
	Projects           []string            `json:"projects"`
	Apps               []appStatus         `json:"applications"`
	Collector          collector.Status    `json:"collector"`
	CaptureDiagnostics []captureDiagnostic `json:"capture_diagnostics,omitempty"`
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
}

func runStatusCommand(args []string, stdout, stderr io.Writer, env Env) int {
	view, err := readStatus(env)
	if err != nil {
		fmt.Fprintf(stderr, "Cannot read archive status: %v\n", err)
		return 1
	}
	if containsString(args, "--json") {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(view); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Agent Archive — %s\n\n", view.State)
	if view.Storage != "" {
		fmt.Fprintf(stdout, "Storage:       %s\nAccess checked: %s\n", view.Storage, formatTimeOrNever(view.StorageVerifiedAt))
		printBucketPrivacy(stdout, view.PrivacyEvidence)
	}
	authentication := fmt.Sprintf("%s (checked %s", view.Authentication.State, formatTimeOrNever(view.Authentication.CheckedAt))
	if view.Authentication.Context != "" {
		authentication += "; " + view.Authentication.Context
	}
	fmt.Fprintf(stdout, "Authentication: %s)\n", authentication)
	fmt.Fprintf(stdout, "Background:    %s\n", view.Background)
	if view.Paused {
		fmt.Fprintln(stdout, "Collection:    paused")
	}
	fmt.Fprintf(stdout, "Projects:      %d included\nPending:       %d session(s)\nLast scan:     %s\nLast publish:  %s\n", len(view.Projects), view.Collector.PendingCount, formatTimeOrNever(view.Collector.LastScanAt), formatTimeOrNever(view.Collector.LastPublishedAt))
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("%d session(s), %d waiting to upload", view.ImportedSessions, view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		fmt.Fprintf(stdout, "Imported:      %s\n", imported)
	}
	for _, app := range view.Apps {
		gaps := ""
		if len(app.CaptureGaps) > 0 {
			gaps = fmt.Sprintf("; %d with a capture gap", len(app.CaptureGaps))
		}
		fmt.Fprintf(stdout, "%s: %s (%d session(s)%s); hooks %s\n", appName(app.Name), app.State, app.Sessions, gaps, app.Hooks)
		if app.Trust == "unknown" {
			fmt.Fprintln(stdout, "  Hook trust: unknown here; it is granted inside the app and is not observable from this Mac's files.")
		}
		fmt.Fprintf(stdout, "  Installed version: %s; support %s%s.\n", installedVersionLabel(app), app.VersionSupport, versionSupportNote(app))
		if app.Capabilities.FreshStart.State == "unavailable" {
			fmt.Fprintf(stdout, "  Fresh-start capture: unavailable. %s\n", app.Capabilities.FreshStart.NextAction)
		}
		for _, pair := range app.Projects {
			fmt.Fprintf(stdout, "  Project %s: %s.\n", pair.ProjectRoot, pair.VerificationState)
		}
		switch {
		case app.ReadBackVerified && !app.VerifiedAt.IsZero():
			fmt.Fprintf(stdout, "  Read-back verified: %s; evidence is for that publication.\n", formatTimeOrNever(app.VerifiedAt))
		case !app.VerifiedAt.IsZero():
			// Some evidence exists but not every project (or session) is
			// covered, so do not call the app verified on the line below
			// its "read-back pending" state.
			fmt.Fprintf(stdout, "  Last read-back: %s (%s).\n", formatTimeOrNever(app.VerifiedAt), readBackProgress(app))
		}
		if app.VerificationDetail != "" {
			fmt.Fprintf(stdout, "  Read-back: %s\n", app.VerificationDetail)
		}
		if len(app.CaptureGaps) > 0 {
			fmt.Fprintf(stdout, "  Capture gaps: %d; see status --json for details.\n", len(app.CaptureGaps))
		}
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		fmt.Fprintf(stdout, "Capture skipped in %s (%s): %s at %s.\n", diagnostic.ProjectRoot, appName(diagnostic.Harness), captureDiagnosticMessage(diagnostic.Code), formatTimeOrNever(diagnostic.ObservedAt))
	}
	if view.Collector.LastError != "" {
		fmt.Fprintf(stdout, "Last error:    %s\n", view.Collector.LastError)
	}
	for _, warning := range view.Warnings {
		fmt.Fprintf(stdout, "Warning:       %s\n", warning)
	}
	fmt.Fprintf(stdout, "\nNext: %s\n", view.Next)
	return 0
}

// blockedReasonDetail explains a recorded capture gap. A missing transcript is
// the ordinary end of an archived session's local life, not a defect, and it
// is the one reason that can end on its own, so it does not borrow the
// permanent wording the other reasons need.
func blockedReasonDetail(reason collector.BlockedReason) string {
	switch reason {
	case collector.BlockedReasonTranscriptMissing:
		return "The application has deleted its own transcript, as each one does on its own schedule. The last published snapshot stays retained and readable, and capture resumes by itself if the file returns."
	case collector.BlockedReasonRecordTooLarge:
		return fmt.Sprintf("One record in the transcript (or a plain-text transcript as a whole) is larger than the %d MiB record size limit, so the transcript cannot be read. The last published snapshot, if any, stays retained, and capture resumes when the transcript changes.", archive.MaxRecordBytes>>20)
	}
	return "The current transcript can no longer be captured; the last published snapshot, if any, stays retained."
}

// versionSupportNote explains an unverified installed version in the text
// status without changing the support state or reason code.
func versionSupportNote(app appStatus) string {
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
	view = statusView{Version: 3, State: "Not set up", Privacy: "not_verified", Background: "unknown", Projects: []string{}, Apps: []appStatus{}, Next: "Run agent-archive setup to get started."}
	home, err := env.readHome()
	if err != nil {
		return view, err
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return view, err
	}
	if _, err := os.Stat(filepath.Join(home, "setup-draft.json")); err == nil {
		view.State = "Setup saved"
		view.Next = "Run agent-archive setup to continue your saved choices."
	}
	if transactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation."
	}
	if !found {
		return view, nil
	}
	view.Storage = fmt.Sprintf("%s / %s / %s", cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix)
	view.StorageVerifiedAt = cfg.StorageVerifiedAt
	view.PrivacyEvidence = currentBucketPrivacy(cfg, env.now())
	view.Privacy = view.PrivacyEvidence.State
	view.ConfigurationID = configurationID(cfg)
	view.CaptureDiagnostics, err = readCaptureDiagnostics(home)
	if err != nil {
		return view, err
	}
	view.CaptureDiagnostics = includedCaptureDiagnostics(view.CaptureDiagnostics, cfg.Archive.Projects)
	view.Authentication.State = "unknown"
	if err := local.Read(filepath.Join(home, "storage-health.json"), &view.Authentication); err != nil && !os.IsNotExist(err) {
		return view, err
	}
	if view.Authentication.ConfigurationID != "" && view.Authentication.ConfigurationID != view.ConfigurationID {
		view.Authentication.State = "stale_configuration"
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
	store := collector.OpenLocalStoreReadOnly(home)
	view.Collector, err = store.LoadStatus()
	if err != nil {
		return view, err
	}
	queued, err := pendingSessions(home, cfg)
	if err != nil {
		return view, err
	}
	if queued > view.Collector.PendingCount {
		view.Collector.PendingCount = queued
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		return view, err
	}
	view.ImportedSessions, view.ImportedPending, view.ImportedWithIssues, err = importedSessionCounts(store, cfg, regs, view.Collector.SessionIssues)
	if err != nil {
		return view, err
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
		readBackIssue := ""
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
			bundle, at, state, found, err := store.LoadPublished(reg.ArchiveSessionID)
			if err != nil {
				return view, err
			}
			if state == collector.CacheStatusBlocked {
				reason, _, e := store.LoadBlocked(reg.ArchiveSessionID)
				if e != nil {
					return view, e
				}
				app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: string(reason), Detail: blockedReasonDetail(reason)})
			}
			if found {
				if state != collector.CacheStatusBlocked {
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
			if found && state != collector.CacheStatusBlocked && app.LastPublishedAt.IsZero() {
				app.State = "captured locally"
			}
			publishedBundle, actualAt, published, e := store.LoadLastPublished(reg.ArchiveSessionID)
			if e != nil {
				return view, e
			}
			if published {
				at = actualAt

				app.Published = true
				app.PublishedSessions++
				if pair != nil {
					pair.Published = true
					pair.PublishedSessions++
				}
				app.State = "published; read-back pending"
				verification, e := readVerification(home, reg.ArchiveSessionID)
				if e != nil {
					return view, e
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
		installed, e := hooks.Installed(userHome, executable, view.Apps[i].Name)
		switch {
		case binaryProblem != "":
			view.Apps[i].Hooks = hooksBroken
		case e != nil || executableErr != nil:
			view.Apps[i].Hooks = "unknown"
		case !installed:
			view.Apps[i].Hooks = "missing or incomplete"
		default:
			view.Apps[i].Hooks = "installed"
		}
	}
	plist := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
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
			case app.Capabilities.FreshStart.State == "unavailable" && !pair.HookObserved:
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
			break
		}
	}
	if view.Background != "running" && view.Background != "loaded" {
		view.State = "Needs attention"
		view.Next = "Run agent-archive setup to restore the background collector."
	}
	if !view.Collector.LastScanAt.IsZero() && env.now().Sub(view.Collector.LastScanAt) > 5*time.Minute {
		view.State = "Needs attention"
		view.Next = "The last scan is over 5 minutes old. Run agent-archive sync to check collection."
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
	if transactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation."
	}
	return view, nil
}

// importedSessionCounts counts the sessions backfill imported, leaving out
// their subagents, and how many of those the collector still has to upload:
// ones it still publishes that have no publication yet (a recorded gap or a
// declined capture aside) or have one in flight. withIssues counts those
// with a recorded capture gap or a failed last scan: status leaves imports
// out of each app's own gaps and issues, so they are reported here.
func importedSessionCounts(store *collector.LocalStore, cfg config.Config, regs []archive.SessionRegistration, issues map[string]string) (imported, pending, withIssues int, err error) {
	for _, reg := range regs {
		if !reg.Imported() || reg.ParentSessionID != "" {
			continue
		}
		imported++
		_, _, state, _, err := store.LoadPublished(reg.ArchiveSessionID)
		if err != nil {
			return 0, 0, 0, err
		}
		if state == collector.CacheStatusBlocked || issues[reg.ArchiveSessionID] != "" {
			withIssues++
		}
		if !cfg.AcceptSession(reg) {
			continue
		}
		_, _, published, err := store.LoadLastPublished(reg.ArchiveSessionID)
		if err != nil {
			return 0, 0, 0, err
		}
		inFlight, err := store.HasPending(reg.ArchiveSessionID)
		if err != nil {
			return 0, 0, 0, err
		}
		if inFlight || (!published && state != collector.CacheStatusBlocked && state != collector.CacheStatusDeclined) {
			pending++
		}
	}
	return imported, pending, withIssues, nil
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
