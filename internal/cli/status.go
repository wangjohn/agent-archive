package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
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
	// readBackFailure is the read-back record VerificationDetail describes,
	// for the text status to word without codes or ISO times.
	readBackFailure verificationEvidence
	Projects        []projectCaptureStatus `json:"projects"`

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
	SkillEvidence            string                 `json:"skill_evidence,omitempty"`
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
	// problem names, in a few words, what Next fixes; the text status leads
	// with it. It is set with Next, and empty when Next is only a tip.
	problem string
	// userHome is the home folder readStatus resolved, for the text status
	// to show paths under it as ~; empty before setup.
	userHome string
}

func runStatusCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("status", stderr)
	jsonOut := fs.Bool("json", false, "print a versioned JSON document")
	verbose := fs.Bool("verbose", false, "also print the codes, exact times and evidence behind each line")
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
	printStatus(stdout, view, statusScreen{style: styleFor(stdout), now: env.now(), home: view.userHome, verbose: *verbose})
	return 0
}

// storageAccessConfirmer is what last confirmed access to the destination,
// as status --json reports it in storage_access_confirmed_by.
type storageAccessConfirmer string

const (
	storageAccessConfirmedBySetup     storageAccessConfirmer = "setup"
	storageAccessConfirmedByCollector storageAccessConfirmer = "collector"
)

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
	readSetupProgress(&view, home)
	if !found {
		return view, nil
	}
	readConfiguredStatus(&view, cfg, home, env)
	store := state.OpenReadOnly(home)
	sessions := readSessionStatus(&view, cfg, home, store)
	for _, name := range cfg.Harnesses {
		view.Apps = append(view.Apps, sessions.appStatus(name, cfg, home, view.Collector.SessionIssues))
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return view, err
	}
	view.userHome = userHome
	binaryProblem := readInstalledApps(&view, cfg, home, userHome, env)
	background := readBackground(&view, cfg, home, userHome, env)
	chooseNextStep(&view, cfg, home, env, binaryProblem, background)
	return view, nil
}

// readSetupProgress reports a saved or interrupted setup, which is all status
// has to say before the first setup commits a configuration.
func readSetupProgress(view *statusView, home string) {
	if _, err := os.Stat(draftPath(home)); err == nil {
		view.State = "Setup saved"
		view.problem = "Setup stopped before it finished"
		view.Next = "Run agent-archive setup to continue your saved choices."
		if _, _, problem, _ := readDraft(home); problem != "" {
			view.problem = "The saved setup can't be used"
			view.Next = fmt.Sprintf("The saved setup in %s cannot be used (%s). Run agent-archive setup: it offers to move it aside and start again.", draftPath(home), problem)
			view.Warnings = append(view.Warnings, view.Next)
		}
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.problem = "Setup was interrupted"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
}

// readConfiguredStatus fills in what the configuration and the advisory
// local files say: storage and its privacy and access evidence, capture
// diagnostics, and the included projects.
func readConfiguredStatus(view *statusView, cfg config.Config, home string, env Env) {
	view.configured = true
	view.SkillEvidence = string(cfg.EffectiveSkillEvidence())
	view.Background = "unknown"
	view.Storage = storageLabel(cfg.Storage)
	view.StorageVerifiedAt = cfg.StorageVerifiedAt
	view.PrivacyEvidence = currentBucketPrivacy(cfg, env.now())
	view.Privacy = view.PrivacyEvidence.State
	view.ConfigurationID = configurationID(cfg)
	// Advisory files: one that cannot be read is left out with a warning,
	// never a reason to report nothing at all.
	var err error
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
}

// statusSessions is the local session state status reads once and then
// attributes to each app: the registrations whose files could be read, and
// skip, which leaves a session whose own files cannot be read out of every
// count, with one warning naming it.
type statusSessions struct {
	store *state.Store
	regs  []archive.SessionRegistration
	skip  func(id string, err error)
}

// readSessionStatus reads the collector's status, the pending count, and the
// import counts, and returns the sessions every app's status is built from.
func readSessionStatus(view *statusView, cfg config.Config, home string, store *state.Store) statusSessions {
	var err error
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
	return statusSessions{store: store, regs: regs, skip: skip}
}

// appStatus is one configured app's capture evidence: its sessions, per
// project and overall, from hook observation to read-back verification.
func (s statusSessions) appStatus(name string, cfg config.Config, home string, issues map[string]string) appStatus {
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
	for _, reg := range s.regs {
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
		s.addSession(&app, pair, reg, cfg, home, issues, &readBackIssue)
	}
	finishReadBack(&app, readBackIssue)
	return app
}

// addSession adds one accepted session's evidence to app and to its project
// pair (nil for a session no configured project owns): hook observation,
// local capture and gaps, publication, and read-back verification. A session
// whose files cannot be read is skipped where it fails.
func (s statusSessions) addSession(app *appStatus, pair *projectCaptureStatus, reg archive.SessionRegistration, cfg config.Config, home string, issues map[string]string, readBackIssue *verificationOutcome) {
	app.Sessions++
	app.HookObserved = true
	gapsBefore := len(app.CaptureGaps)
	if pair != nil {
		pair.HookObserved = true
	}
	if issue := issues[reg.ArchiveSessionID]; issue != "" {
		app.CaptureGaps = append(app.CaptureGaps, archive.CaptureGap{Code: issue, Detail: "Last scan could not update this session; retained evidence was kept. Run agent-archive sync for the failure."})
	}
	if reg.Harness.Version != "" && !containsString(app.HarnessVersions, reg.Harness.Version) {
		app.HarnessVersions = append(app.HarnessVersions, reg.Harness.Version)
	}
	if app.State == "waiting for first session" {
		app.State = "hook observed; waiting for capture"
	}
	bundle, _, cacheStatus, found, err := s.store.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		s.skip(reg.ArchiveSessionID, err)
		return
	}
	if cacheStatus == state.CacheStatusBlocked {
		reason, _, e := s.store.LoadBlocked(reg.ArchiveSessionID)
		if e != nil {
			s.skip(reg.ArchiveSessionID, e)
			return
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
	s.addPublication(app, pair, reg, cfg, home, readBackIssue)
}

// addPublication adds a session's last publication, if any, and its
// read-back verification to app and pair. readBackIssue keeps the worst
// read-back failure seen for the app: a mismatch outranks a transient one.
func (s statusSessions) addPublication(app *appStatus, pair *projectCaptureStatus, reg archive.SessionRegistration, cfg config.Config, home string, readBackIssue *verificationOutcome) {
	publishedBundle, at, published, e := s.store.LoadLastPublished(reg.ArchiveSessionID)
	if e != nil {
		s.skip(reg.ArchiveSessionID, e)
		return
	}
	if !published {
		return
	}
	app.Published = true
	app.PublishedSessions++
	if pair != nil {
		pair.Published = true
		pair.PublishedSessions++
	}
	app.State = "published; read-back pending"
	verification, e := readVerification(home, reg.ArchiveSessionID)
	if e != nil {
		s.skip(reg.ArchiveSessionID, e)
		return
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
		if verification.Outcome == verificationOutcomeMismatch || *readBackIssue != verificationOutcomeMismatch {
			*readBackIssue = verification.Outcome
			app.readBackFailure = verification
			app.VerificationDetail = fmt.Sprintf("%s: %s (attempt %d; next retry %s)", verification.Outcome, verification.LastError, verification.Attempts, formatTimeOrNever(verification.NextRetryAt))
		}
	}
	if at.After(app.LastPublishedAt) {
		app.LastPublishedAt = at
	}
}

// finishReadBack decides, once every session is counted, whether each of
// app's projects and the app as a whole are read-back verified.
func finishReadBack(app *appStatus, readBackIssue verificationOutcome) {
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
}

// readInstalledApps fills in each app's installed version, support, and
// hook state, and returns what is wrong with the executable setup installed
// ("" when nothing is).
func readInstalledApps(view *statusView, cfg config.Config, home, userHome string, env Env) (binaryProblem string) {
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
	return binaryProblem
}

// statusBackground is the background collector as status found it: its
// LaunchAgent's plist, and the program the LaunchAgent runs when that
// program is no longer usable (problem says why; both "" otherwise).
// environmentProblems say why the environment the LaunchAgent sets cannot
// load the configured S3 profile.
type statusBackground struct {
	plist               string
	program             string
	problem             string
	environmentProblems []string
}

// readBackground reads the background collector's launchd state, and what
// the LaunchAgent actually runs.
func readBackground(view *statusView, cfg config.Config, home, userHome string, env Env) statusBackground {
	plist := env.installation(home, userHome).installedCollectorPlist()
	view.Background = env.jobState(plist)
	// launchd reports a job whose program is gone as loaded (it only fails
	// when it fires), so read the program the LaunchAgent actually runs.
	backgroundProgram, backgroundProblem := "", ""
	var environmentProblems []string
	if cfg.Archive.Enabled {
		if data, err := os.ReadFile(plist); err == nil {
			if program, err := hooks.LaunchAgentProgram(data); err == nil {
				backgroundProgram, backgroundProblem = program, executableProblem(program)
			}
			// The collector has only the environment its plist sets, which
			// may no longer match the files and programs the profile needs.
			if environment, err := hooks.LaunchAgentEnvironment(data); err == nil {
				environmentProblems = collectorEnvironmentProblems(cfg.Storage, environment, userHome)
				view.Warnings = append(view.Warnings, environmentProblems...)
				if drift := env.awsFilesDrift(cfg.Storage, environment, userHome); drift != "" {
					view.Warnings = append(view.Warnings, drift)
				}
			}
		}
	}
	if backgroundProblem != "" {
		view.Background = backgroundBroken
		view.Warnings = append(view.Warnings, fmt.Sprintf("The background collector's LaunchAgent runs %s, which is %s, so scheduled collection has stopped.", backgroundProgram, backgroundProblem))
	}
	return statusBackground{plist: plist, program: backgroundProgram, problem: backgroundProblem, environmentProblems: environmentProblems}
}

// scanStaleAfter is how old the last scan may be before status says
// collection needs attention.
const scanStaleAfter = 5 * time.Minute

// chooseNextStep sets the overall state and the one next step status
// suggests. Later checks outrank earlier ones: each overwrites the state and
// step of any before it.
func chooseNextStep(view *statusView, cfg config.Config, home string, env Env, binaryProblem string, background statusBackground) {
	view.State = "Ready"
	view.problem = ""
	view.Next = "Keep working. Run agent-archive list to inspect archived sessions."
	if len(view.Apps) == 0 {
		view.State = "Needs attention"
		view.problem = "No apps are selected"
		view.Next = "Run agent-archive setup and select at least one application."
	} else if len(view.Projects) == 0 {
		view.State = "Needs attention"
		view.problem = "No projects are included"
		view.Next = "Run agent-archive setup and include at least one project."
	}
	chooseCaptureStep(view)
	chooseInstallationStep(view, background.plist)
	if !view.Collector.LastScanAt.IsZero() && env.now().Sub(view.Collector.LastScanAt) > scanStaleAfter {
		view.State = "Needs attention"
		view.problem = "No scan in over 5 minutes"
		view.Next = "The last scan is over 5 minutes old. Run agent-archive sync to check collection."
	}
	// sync cannot help while another process holds the collector lock. The
	// holder records when it took the lock, so a pass that started a moment
	// ago is never mistaken for a hung one.
	if record, ok := readCollectorLockRecord(home); ok && env.now().Sub(record.Since) > collectLockStuckAfter && processAlive(record.PID) && collectorLockHeld(home) {
		view.State = "Needs attention"
		view.problem = "Collection is stuck"
		view.Next = fmt.Sprintf("Collection is stuck: %s (process %d) has held the collector lock since %s, %s, well past a pass's time limit. If that command is no longer doing anything, quit process %d (in Activity Monitor or with kill %d); the next pass then resumes.", record.Holder, record.PID, record.Since.UTC().Format("2006-01-02 15:04 UTC"), durationAgo(env.now().Sub(record.Since)), record.PID, record.PID)
	}
	if view.Collector.LastError != "" {
		view.State = "Needs attention"
		view.problem = "The last sync failed"
		view.Next = "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage."
		// A Keychain failure has one specific fix; status.json keeps only
		// the error text, so it is recognized from that.
		if action := credentials.RecoveryActionForMessage(view.Collector.LastError); action != "" {
			view.Next = action
		}
		if strings.Contains(view.Collector.LastError, backgroundCredentialProcessFailure) {
			view.problem = "The background collector couldn't get AWS credentials"
			view.Next = "The background collector couldn't get credentials from your AWS profile's credential_process. It runs without most of your shell's environment: if the helper needs a setting the collector doesn't get (see Environment variables in the configuration reference), put it in the helper's own configuration; if it needs you to unlock it or sign in, do that. Check with agent-archive sync, then run agent-archive setup again from a shell where it works."
		}
	}
	if len(background.environmentProblems) > 0 {
		view.State = "Needs attention"
		view.problem = "The background collector can't load your AWS profile"
		view.Next = "The background collector cannot load your AWS profile (see the warning above). Run agent-archive setup again from a shell where the profile works, so the collector gets that shell's AWS settings files and PATH."
	}
	if cfg.Paused {
		view.State = "Paused"
		view.problem = "Collection is paused"
		view.Next = "Run agent-archive resume when ready. Registered sessions can catch up after resume."
	}
	// A moved or deleted binary is the root cause of every symptom above (a
	// stale scan, a failing hook), and pausing does not stop the apps from
	// running hooks that now fail, so it outranks all of them.
	if binaryProblem != "" || background.problem != "" {
		moved := cfg.InstalledExecutable
		if binaryProblem == "" {
			moved = background.program
		}
		view.State = "Needs attention"
		view.problem = "agent-archive can't run from where setup installed it"
		view.Next = fmt.Sprintf("agent-archive is no longer usable at %s. Run agent-archive setup from the binary's new location to point the hooks and background collector at it.", moved)
	}
	if !cfg.Archive.Enabled {
		view.State = "Not installed"
		view.problem = "Agent Archive is uninstalled"
		view.Next = "Local data is kept. Run agent-archive setup to reinstall."
	}
	if setupjournal.TransactionPending(home) {
		view.State = "Setup needs recovery"
		view.problem = "Setup was interrupted"
		view.Next = "Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now."
	}
}

// chooseCaptureStep points at the first project of the first app whose
// capture is not yet read-back verified, if any.
func chooseCaptureStep(view *statusView) {
	for _, app := range view.Apps {
		for _, pair := range app.Projects {
			if pair.ReadBackVerified {
				continue
			}
			view.State = "Waiting for capture"
			view.problem = "Waiting for the first " + appName(app.Name) + " session"
			if app.VerifiedSessions > 0 {
				// The app is captured elsewhere; only this project is new.
				view.problem = "Waiting for a " + appName(app.Name) + " session in " + pair.ProjectRoot
			}
			switch {
			case app.Capabilities.FreshStart.State == capabilityUnavailable && !pair.HookObserved:
				view.Next = app.Capabilities.FreshStart.NextAction
			case pair.Published:
				view.problem = appName(app.Name) + "'s upload hasn't been read back yet"
				view.Next = "Run agent-archive sync to retry read-back verification for " + appName(app.Name) + " in " + pair.ProjectRoot + "."
			case pair.HookObserved:
				view.problem = appName(app.Name) + " session seen but not uploaded yet"
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
}

// chooseInstallationStep points at hooks that are not installed, then at a
// background collector that is not loaded (plist is its LaunchAgent).
func chooseInstallationStep(view *statusView, plist string) {
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			view.State = "Needs attention"
			view.problem = appName(app.Name) + " hooks aren't installed"
			if app.Hooks == "unknown" {
				view.problem = appName(app.Name) + " hooks couldn't be checked"
			}
			view.Next = "Run agent-archive setup to check the hooks for " + appName(app.Name) + "."
			if len(app.OtherInstallations) > 0 {
				// setup refuses to install beside them, so it is not the way out.
				view.problem = "Another installation's hooks are in " + appName(app.Name)
				view.Next = "Another agent-archive installation's hooks are in " + appName(app.Name) + "'s hook file (see the warning above). Remove that installation, or give this one its own HOME, then run agent-archive setup."
			}
			break
		}
	}
	if !setupjournal.JobActive(view.Background) {
		view.State = "Needs attention"
		view.problem = "The background collector isn't running"
		if view.Background == "unknown" {
			view.problem = "The background collector couldn't be checked"
		}
		view.Next = "Run agent-archive setup to restore the background collector."
		if view.Background == setupjournal.JobAnotherInstallation {
			// setup refuses to replace that job, so it is not the way out.
			view.problem = "Another installation's collector has this installation's label"
			view.Next = fmt.Sprintf("Another agent-archive installation's collector runs under this installation's launchd label (%s), and setup will not replace it. Set AGENT_ARCHIVE_HOME to a data directory of this installation's own, or uninstall the other installation.", launchLabel(plist))
		}
	}
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

// statusScreen is what the text status is drawn with: the output's style,
// the clock times are shown relative to, the home folder shown as ~, and
// whether to add the Details section (status --verbose).
type statusScreen struct {
	style   textStyle
	now     time.Time
	home    string
	verbose bool
}

// printStatus writes the text status: the overall state, what to do about
// it, the Capture and Storage sections, and anything else worth knowing.
// Codes, exact times and raw errors are left to the Details section that
// status --verbose adds, and to status --json.
func printStatus(out io.Writer, view statusView, sc statusScreen) {
	s := sc.style
	terminal.Printf(out, "%s  %s\n", s.bold("Agent Archive"), sc.stateLabel(view.State))
	if view.State != "Ready" {
		terminal.Println(out)
		sc.printNextStep(out, view)
	}
	if !view.configured {
		return
	}
	terminal.Printf(out, "\n%s\n", s.bold("Capture"))
	sc.printRows(out, sc.captureRows(view))
	terminal.Printf(out, "\n%s\n", s.bold("Storage"))
	sc.printRows(out, sc.storageRows(view))
	if notes := sc.noteRows(view); len(notes) > 0 {
		terminal.Printf(out, "\n%s\n", s.bold("Notes"))
		sc.printRows(out, notes)
	}
	if sc.verbose {
		terminal.Printf(out, "\n%s\n", s.bold("Details"))
		printStatusDetails(out, view)
	}
	terminal.Println(out)
	if view.State == "Ready" {
		for i, line := range sc.proseLines(view.Next) {
			lead := s.dim("Next:") + " "
			if i > 0 {
				lead = "      "
			}
			terminal.Println(out, s.hang(lead, line))
		}
	}
	if sc.verbose {
		terminal.Printf(out, "%s %s\n", s.dim("As JSON:"), s.cmd("agent-archive status --json"))
		return
	}
	terminal.Printf(out, "%s %s\n", s.dim("Details:"), s.cmd("agent-archive status --verbose"))
}

// stateLabel is the overall state after a dot, green when all is well and
// yellow when it needs the user.
func (sc statusScreen) stateLabel(state string) string {
	label := "● " + state
	//lint:ignore LV1001 the overall state is an open-ended label (statusView.State); statusCode maps it to the stable code
	switch state {
	case "Ready":
		return sc.style.ok(label)
	case "Not installed":
		return sc.style.dim(label)
	}
	return sc.style.warn(label)
}

// printNextStep writes the one thing to do now: what is wrong, and under it
// how to fix it.
func (sc statusScreen) printNextStep(out io.Writer, view statusView) {
	indent := "    "
	if view.problem == "" {
		indent = "  "
	} else {
		terminal.Println(out, sc.style.hang("  "+sc.style.warnMark()+" ", sc.tilde(view.problem)))
	}
	for _, line := range sc.proseLines(view.Next) {
		terminal.Println(out, sc.style.hang(indent, line))
	}
}

// statusRow is one line of a status section: a colored symbol, columns
// aligned with the section's other rows, dim detail after them, and notes on
// the lines below.
type statusRow struct {
	mark   string
	cells  []string
	detail string
	notes  []statusNote
}

// statusNote is a line under a row: an optional symbol and its text.
type statusNote struct {
	mark string
	text string
}

// info is the symbol of a row that is only information.
func (sc statusScreen) info() string { return sc.style.dim("·") }

// printRows writes rows, aligning the columns of those that have more than
// one column or a detail. On a terminal too narrow for a row's detail beside
// its columns, the detail goes on the lines under it.
func (sc statusScreen) printRows(out io.Writer, rows []statusRow) {
	var widths []int
	for _, row := range rows {
		for i, cell := range row.cells {
			if i == len(row.cells)-1 && row.detail == "" {
				break
			}
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	for _, row := range rows {
		var columns strings.Builder
		columns.WriteString("  " + row.mark + " ")
		text := ""
		for i, cell := range row.cells {
			if i == len(row.cells)-1 && row.detail == "" {
				text = cell
				break
			}
			columns.WriteString(cell + strings.Repeat(" ", widths[i]-visibleWidth(cell)+3))
		}
		prefix := columns.String()
		if row.detail != "" {
			text = sc.style.dim(row.detail)
		}
		if sc.style.width > 0 && row.detail != "" && visibleWidth(prefix+text) > sc.style.width {
			// Too narrow for the detail beside the columns: it goes under them.
			terminal.Println(out, strings.TrimRight(prefix, " "))
			prefix = "    "
		}
		terminal.Println(out, strings.TrimRight(sc.style.hang(prefix, text), " "))
		for _, note := range row.notes {
			lead := "    "
			if note.mark != "" {
				lead += note.mark + " "
			}
			terminal.Println(out, sc.style.hang(lead, note.text))
		}
	}
}

// captureRows are the Capture section: one row per app, then what capture
// skipped, the included projects, and imports.
func (sc statusScreen) captureRows(view statusView) []statusRow {
	var rows []statusRow
	for _, app := range view.Apps {
		rows = append(rows, sc.appRow(app))
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{fmt.Sprintf("%s skipped a session in %s %s: %s", appName(diagnostic.Harness), sc.path(diagnostic.ProjectRoot), relativeAge(sc.now, diagnostic.ObservedAt), capture.DiagnosticMessage(diagnostic.Code))}})
	}
	projects := "none included"
	if len(view.Projects) > 0 {
		shown := make([]string, len(view.Projects))
		for i, project := range view.Projects {
			shown[i] = sc.path(project)
		}
		projects = strings.Join(shown, ", ")
	}
	rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Projects: " + projects}})
	rows = append(rows, statusRow{mark: sc.info(), cells: []string{"Skill evidence: " + view.SkillEvidence}})
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("Imported: %s, %d waiting to upload", plural(view.ImportedSessions, "session"), view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{imported}})
	}
	return rows
}

// appRow is one app's capture: its hooks, how far its sessions have got,
// and anything that needs a closer look.
func (sc statusScreen) appRow(app appStatus) statusRow {
	s := sc.style
	name := appName(app.Name)
	if app.InstalledVersion != "" {
		name += " " + app.InstalledVersion
	}
	row := statusRow{mark: s.okMark(), cells: []string{name, hooksLabel(app.Hooks)}}
	switch {
	case app.Hooks == hooksBroken:
		row.mark = s.failMark()
	case app.Hooks != "installed", app.readBackFailure.Attempts > 0:
		row.mark = s.warnMark()
	}
	//lint:ignore LV1001 an app's state is an open-ended label (appStatus.State); statusCode maps it to the stable code
	switch app.State {
	case "published; source verified":
		row.detail = plural(app.PublishedSessions, "session") + " archived, verified " + relativeAge(sc.now, app.VerifiedAt)
	case "published; read-back pending":
		row.detail = "uploaded, read-back pending"
		if !app.VerifiedAt.IsZero() {
			row.detail += " (" + readBackProgress(app) + ")"
		}
	case "captured locally":
		row.detail = "captured, not uploaded yet"
	case "hook observed; waiting for capture":
		row.detail = "session seen, not captured yet"
	default:
		row.detail = "waiting for first session"
	}
	if approve, ok := hookApproval[app.Name]; ok && app.Hooks == "installed" && !app.HookObserved {
		row.notes = append(row.notes, statusNote{sc.info(), fmt.Sprintf(approve, s.cmd("/hooks"))})
	}
	if app.Capabilities.FreshStart.State == capabilityUnavailable {
		row.notes = append(row.notes, statusNote{s.warnMark(), "New sessions can't be captured yet: " + app.Capabilities.FreshStart.NextAction})
	}
	if len(app.Projects) > 1 && !app.ReadBackVerified {
		for _, pair := range app.Projects {
			mark := sc.info()
			if pair.ReadBackVerified {
				mark = s.okMark()
			}
			row.notes = append(row.notes, statusNote{mark, sc.path(pair.ProjectRoot) + ": " + pairProgress(pair)})
		}
	}
	if failure := app.readBackFailure; failure.Attempts > 0 {
		row.notes = append(row.notes, statusNote{s.warnMark(), sc.readBackFailure(failure)})
	}
	//lint:ignore LV1001 the reason codes are untyped constants in capabilities.go, which computes this field
	switch app.VersionSupportReason {
	case supportReasonNoMatchingVersion:
		row.notes = append(row.notes, statusNote{sc.info(), "Sessions verified so far came from a different " + appName(app.Name) + " version."})
	case supportReasonVersionSourceMismatch:
		row.notes = append(row.notes, statusNote{sc.info(), "The installed version and the sessions' versions are numbered differently, so they can't be compared."})
	}
	if len(app.CaptureGaps) > 0 {
		row.notes = append(row.notes, statusNote{sc.info(), fmt.Sprintf("%s with a capture gap (%s recorded; details in status --json)", plural(app.SessionsWithCaptureGaps, "session"), plural(len(app.CaptureGaps), "gap"))})
	}
	return row
}

// hookApproval explains, for an app that runs new hooks only once the user
// approves them (hookNextStep), how to, until its first session shows they
// run. %s is the command.
var hookApproval = map[string]string{
	"codex": "Run %s in Codex and approve the archive hooks; agent-archive can't see whether you have.",
}

// hooksLabel words an app's hook state.
func hooksLabel(state string) string {
	//lint:ignore LV1001 appStatus.Hooks is a plain string, set as literals in readInstalledApps
	switch state {
	case "missing or incomplete":
		return "hooks missing"
	case "unknown":
		return "hooks not checked"
	}
	return "hooks " + state
}

// pairProgress words how far one project's capture has got.
func pairProgress(pair projectCaptureStatus) string {
	switch {
	case pair.ReadBackVerified:
		return "verified"
	case pair.Published:
		return "uploaded, read-back pending"
	case pair.CapturedLocally:
		return "captured, not uploaded yet"
	case pair.HookObserved:
		return "session seen, not captured yet"
	}
	return "waiting for first session"
}

// readBackFailure words a publication that could not be read back, and
// when the collector tries again.
func (sc statusScreen) readBackFailure(failure verificationEvidence) string {
	text := "Read-back failed"
	if failure.Outcome == verificationOutcomeMismatch {
		text = "Read-back doesn't match what this Mac uploaded"
	}
	if failure.LastError != "" {
		text += ": " + failure.LastError
	}
	retry := "retrying on the next pass"
	if failure.NextRetryAt.After(sc.now) {
		retry = "retrying in " + strings.TrimSuffix(relativeAge(failure.NextRetryAt, sc.now), " ago")
		retry = strings.Replace(retry, "in just now", "in under a minute", 1)
	}
	return fmt.Sprintf("%s (%s, %s so far)", text, retry, plural(failure.Attempts, "attempt"))
}

// storageRows are the Storage section: the destination, the bucket's
// privacy, the background collector, the last error, and uploads.
func (sc statusScreen) storageRows(view statusView) []statusRow {
	var rows []statusRow
	if view.Storage != "" {
		rows = append(rows, sc.destinationRow(view), sc.privacyRow(view.PrivacyEvidence))
	}
	rows = append(rows, sc.backgroundRow(view))
	for _, text := range lastErrorRows(view.Collector) {
		rows = append(rows, statusRow{mark: sc.style.failMark(), cells: []string{text}})
	}
	uploads := fmt.Sprintf("Last upload: %s · %d pending", sc.ago(view.Collector.LastPublishedAt), view.Collector.PendingCount)
	rows = append(rows, statusRow{mark: sc.info(), cells: []string{uploads}})
	return rows
}

// destinationRow is where sessions go, and whether storage was last found
// reachable with the configured credentials.
func (sc statusScreen) destinationRow(view statusView) statusRow {
	s := sc.style
	row := statusRow{cells: []string{storageURL(view.Storage)}}
	auth := view.Authentication
	checked := ""
	if !auth.CheckedAt.IsZero() {
		checked = ", checked " + relativeAge(sc.now, auth.CheckedAt)
	}
	// A storage check newer than the recorded health (setup's, after new
	// credentials) outranks it: the health is then out of date, not wrong now.
	health := auth.State
	if view.StorageAccessConfirmedAt.After(auth.CheckedAt) {
		health = ""
	}
	//lint:ignore LV1001 storageHealth.State is an untyped string set in collect.go and verification.go
	switch health {
	case "authentication_failed":
		row.mark, row.detail = s.failMark(), "sign-in failed"+checked
	case "credentials_expired":
		row.mark, row.detail = s.failMark(), "credentials expired"+checked
	case "credentials_unavailable":
		row.mark, row.detail = s.failMark(), "credentials unavailable"+checked
	case "storage_unavailable":
		row.mark, row.detail = s.failMark(), "unreachable"+checked
	case "stale_configuration":
		row.mark, row.detail = s.warnMark(), "storage settings changed since the last check"
	case "stale":
		row.mark, row.detail = s.warnMark(), "last reachable "+relativeAge(sc.now, auth.CheckedAt)+"; the collector checks every few minutes"
	}
	if row.mark != "" {
		return row
	}
	switch view.StorageAccessConfirmedBy {
	case storageAccessConfirmedByCollector:
		row.mark, row.detail = s.okMark(), "reachable, checked "+relativeAge(sc.now, view.StorageAccessConfirmedAt)
	case storageAccessConfirmedBySetup:
		row.mark, row.detail = s.okMark(), "reachable, checked by setup "+relativeAge(sc.now, view.StorageAccessConfirmedAt)
	default:
		row.mark, row.detail = sc.info(), "not checked yet"
	}
	return row
}

// storageURL shows a "provider / bucket / prefix" label as
// provider://bucket/prefix.
func storageURL(label string) string {
	parts := strings.SplitN(label, " / ", 3)
	if len(parts) < 2 {
		return label
	}
	return parts[0] + "://" + strings.Join(parts[1:], "/")
}

// privacyRow is what the last inspection found about the bucket's public
// access, with the provider's guidance when it is not known to be private.
func (sc statusScreen) privacyRow(report storage.PrivacyReport) statusRow {
	s := sc.style
	checked := ""
	if report.CheckedAt != nil {
		checked = ", checked " + relativeAge(sc.now, *report.CheckedAt)
	}
	review := statusNote{text: "Check public access: " + s.cmd(report.GuidanceURL)}
	//lint:ignore LV1001 storage.PrivacyReport.State is an untyped string owned by package storage
	switch report.State {
	case "verified_private":
		return statusRow{mark: s.okMark(), cells: []string{"Bucket is private"}, detail: "public access blocked" + checked}
	case "public_or_risky":
		detail := "public access is allowed"
		//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
		switch report.Reason {
		case "public_bucket_policy":
			detail = "its bucket policy is public"
		case "public_bucket_acl":
			detail = "its access list grants public access"
		}
		return statusRow{mark: s.failMark(), cells: []string{"Bucket may be public"}, detail: detail + checked, notes: []statusNote{review}}
	}
	detail := ""
	//lint:ignore LV1001 storage.PrivacyReport.Reason is an untyped string owned by package storage
	switch report.Reason {
	case "inspection_unavailable":
		detail = "this storage can't be inspected"
	case "r2_management_credentials_not_configured":
		detail = "R2 object credentials can't inspect public access"
	case "public_access_controls_not_fully_verified":
		detail = "some public access settings couldn't be read" + checked
	case "inspection_stale":
		detail = "the last check is over a day old" + checked
		if report.CheckedAt != nil && report.CheckedAt.After(sc.now) {
			detail = "the last check is dated in the future; the clock may have changed"
		}
	case "storage_configuration_changed":
		detail = "storage settings changed since the last check"
	}
	return statusRow{mark: s.warnMark(), cells: []string{"Bucket privacy not verified"}, detail: detail, notes: []statusNote{review}}
}

// backgroundRow is the background collector: whether launchd runs it, and
// when it last scanned.
func (sc statusScreen) backgroundRow(view statusView) statusRow {
	s := sc.style
	scan := "last scan " + sc.ago(view.Collector.LastScanAt)
	if view.Collector.LastScanAt.IsZero() {
		scan = "no scan yet"
	}
	switch {
	case view.Background == backgroundBroken:
		return statusRow{mark: s.failMark(), cells: []string{"Background collector is broken"}, detail: "the program it runs is gone or can't be run"}
	case view.Background == setupjournal.JobAnotherInstallation:
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector belongs to another installation"}}
	case view.Background == "unknown":
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector state unknown"}, detail: "launchctl couldn't say"}
	case !setupjournal.JobActive(view.Background):
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector isn't running"}}
	case view.Paused:
		return statusRow{mark: sc.info(), cells: []string{"Background collector paused"}, detail: scan}
	case !view.Collector.LastScanAt.IsZero() && sc.now.Sub(view.Collector.LastScanAt) > scanStaleAfter:
		return statusRow{mark: s.warnMark(), cells: []string{"Background collector on"}, detail: scan}
	}
	return statusRow{mark: s.okMark(), cells: []string{"Background collector on"}, detail: scan}
}

// noteRows are what the collector could not read or refresh, and status's
// warnings.
func (sc statusScreen) noteRows(view statusView) []statusRow {
	var rows []statusRow
	if n := len(view.Collector.QuarantinedFiles); n > 0 {
		text := "1 local state file couldn't be read and was moved aside; its session keeps its other evidence."
		if n > 1 {
			text = fmt.Sprintf("%d local state files couldn't be read and were moved aside; their sessions keep their other evidence.", n)
		}
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{text + " See status --json for the files, then delete them."}})
	}
	if n := view.Collector.UnrefreshableSummaries; n > 0 {
		text := "1 session summary can't be refreshed by this version and stays as published until the session changes."
		if n > 1 {
			text = fmt.Sprintf("%d session summaries can't be refreshed by this version and stay as published until their sessions change.", n)
		}
		rows = append(rows, statusRow{mark: sc.info(), cells: []string{text}})
	}
	for _, warning := range view.Warnings {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{sc.tilde(warning)}})
	}
	return rows
}

// ago is how long ago t was, or "never".
func (sc statusScreen) ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return relativeAge(sc.now, t)
}

// path shows path with the home folder as ~.
func (sc statusScreen) path(path string) string {
	if sc.home == "" {
		return path
	}
	return displayPath(path, sc.home)
}

// tilde shows the paths under the home folder in text as ~ paths.
func (sc statusScreen) tilde(text string) string {
	if sc.home == "" {
		return text
	}
	home := filepath.Clean(sc.home) + string(filepath.Separator)
	if home == "//" {
		return text // every path is under /; ~ would only obscure them
	}
	var b strings.Builder
	for {
		i := strings.Index(text, home)
		if i < 0 {
			break
		}
		// Only a path that starts at the home folder: not one that merely
		// contains it, like /Volumes/Backup/Users/jo/.
		if i > 0 && pathByte(text[i-1]) {
			b.WriteString(text[:i+1])
			text = text[i+1:]
			continue
		}
		b.WriteString(text[:i] + "~" + string(filepath.Separator))
		text = text[i+len(home):]
	}
	return b.String() + text
}

// pathByte reports whether c can be part of a path, so that a home folder
// right after it is not where the path starts.
func pathByte(c byte) bool {
	return c == '/' || c == '.' || c == '_' || c == '-' || c == '~' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// prose readies a next step for the screen: ~ paths, the warnings it refers
// to are below it rather than above, and its first command is highlighted,
// the one to run.
func (sc statusScreen) prose(text string) string {
	text = strings.ReplaceAll(sc.tilde(text), "the warning above", "the warning below")
	if loc := proseCommand.FindStringIndex(text); loc != nil {
		text = text[:loc[0]] + sc.style.cmd(text[loc[0]:loc[1]]) + text[loc[1]:]
	}
	return text
}

// proseLines readies a next step for the screen as prose does, one line for
// each sentence that names a command, so that every command is highlighted
// and no line highlights two. A sentence that names two commands is split
// after the comma or semicolon before the second. A sentence without a
// command stays on the line before it.
func (sc statusScreen) proseLines(text string) []string {
	var pieces []string
	for _, sentence := range sentences(text) {
		pieces = append(pieces, clauses(sentence)...)
	}
	var lines []string
	line := ""
	for _, sentence := range pieces {
		if line != "" && proseCommand.MatchString(line) && proseCommand.MatchString(sentence) {
			lines = append(lines, sc.prose(line))
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += sentence
	}
	return append(lines, sc.prose(line))
}

// sentences splits text after each full stop that a space and a capital
// letter follow.
func sentences(text string) []string {
	var out []string
	for {
		i := sentenceEnd.FindStringIndex(text)
		if i == nil {
			return append(out, text)
		}
		out = append(out, text[:i[0]+1])
		text = text[i[1]-1:]
	}
}

var sentenceEnd = regexp.MustCompile(`\. [A-Z]`)

// clauses splits a sentence that names more than one command after the last
// ", " or "; " before each command but the first, so each part names one.
// A command with no such break before it stays with the one before it.
func clauses(sentence string) []string {
	commands := proseCommand.FindAllStringIndex(sentence, -1)
	var out []string
	start := 0
	for i := 1; i < len(commands); i++ {
		between := sentence[commands[i-1][1]:commands[i][0]]
		cut := max(strings.LastIndex(between, ", "), strings.LastIndex(between, "; "))
		if cut < 0 {
			continue
		}
		cut += commands[i-1][1] + 1
		out = append(out, sentence[start:cut])
		start = cut + 1
	}
	return append(out, sentence[start:])
}

// proseCommand matches an agent-archive command, with its flags, in a
// sentence.
var proseCommand = regexp.MustCompile(`agent-archive (?:` + strings.Join(slices.Sorted(maps.Keys(commandHelp)), "|") + `)\b(?: --[a-z][a-z-]*)*`)

// lastErrorRows are the Storage section's lines for the collector's last
// errors, one per problem the pass recorded. A storage refusal is shown by
// its plain cause (see storage.Diagnose); the raw error text is in status
// --verbose. Anything else, like the collector's own counts of failed
// sessions, is shown as recorded.
//
// A status file written before Status.LastErrors existed has only the joined
// LastError. It is split where the collector joined problems, on "; ", and
// shown on one line as before (see legacyLastErrorText).
func lastErrorRows(status state.Status) []string {
	if len(status.LastErrors) == 0 {
		if status.LastError == "" {
			return nil
		}
		return []string{legacyLastErrorText(status.LastError)}
	}
	rows := make([]string, 0, len(status.LastErrors))
	for _, problem := range status.LastErrors {
		if cause := storageErrorCause(problem); cause != "" {
			rows = append(rows, cause)
			continue
		}
		rows = append(rows, "Last error: "+problem)
	}
	return rows
}

// legacyLastErrorText is the one line an older status file's joined
// lastError is shown as: a lone storage refusal's plain cause, or "Last
// error: " and its problems, each storage refusal among them by its cause.
func legacyLastErrorText(lastError string) string {
	parts := strings.Split(lastError, "; ")
	plain := false
	for i, part := range parts {
		if cause := storageErrorCause(part); cause != "" {
			parts[i], plain = cause, true
		}
	}
	if plain && len(parts) == 1 {
		return parts[0]
	}
	return "Last error: " + strings.Join(parts, "; ")
}

// storageErrorCause names, in a few words, the storage failure an error
// recorded as text reports, or returns "" when it reports none that
// storage.Diagnose recognizes. The status file keeps only the error's text,
// so the provider's error code is read back from where the SDK writes it
// ("api error AccessDenied: Access Denied") and diagnosed as a code; a
// message that merely mentions a code elsewhere is not taken for it.
//
// Like storage.Diagnose, a failure while fetching credentials (an STS, SSO
// or other sign-in service call inside the storage call) is a credential
// problem whatever its code, not a refusal by the bucket. Such text is left
// as recorded, since the code alone would misname it.
func storageErrorCause(text string) string {
	for _, match := range recordedOperationService.FindAllStringSubmatch(text, -1) {
		if match[1] != s3.ServiceID {
			return ""
		}
	}
	for _, match := range recordedErrorCode.FindAllStringSubmatch(text, -1) {
		switch storage.Diagnose(&smithy.GenericAPIError{Code: match[1]}).Cause {
		case storage.CauseAccessDenied:
			return "Storage refused access"
		case storage.CauseNoCredentials:
			return "Storage didn't accept the credentials"
		case storage.CauseNoSuchBucket:
			return "The bucket doesn't exist"
		case storage.CauseWrongRegion:
			return "The bucket is in a different region"
		case storage.CauseNetwork, storage.CauseOther:
			// Not recognized from a code alone.
		}
	}
	return ""
}

// recordedErrorCode matches a provider error code where an error's text puts
// it: after "api error " or a ": ", and before ": ".
var recordedErrorCode = regexp.MustCompile(`(?:^|api error |: )([A-Z][A-Za-z]+): `)

// recordedOperationService matches the service of each SDK call an error's
// text names ("operation error STS: AssumeRole"), the way storage.Diagnose
// reads the chain of operation errors.
var recordedOperationService = regexp.MustCompile(`operation error ([^:]+): `)

// printStatusDetails writes the Details section of status --verbose: every
// line the text status printed before it was redesigned, with its codes,
// exact times, full paths and raw errors.
func printStatusDetails(out io.Writer, view statusView) {
	terminal.Printf(out, "  State:         %s (%s)\n", view.State, view.Code)
	if view.Storage != "" {
		terminal.Printf(out, "  Storage:       %s\n  Access:        %s\n", view.Storage, storageAccessLine(view))
		printBucketPrivacy(out, view.PrivacyEvidence)
	}
	checked := "not checked yet"
	if !view.Authentication.CheckedAt.IsZero() {
		checked = "checked " + formatTimeOrNever(view.Authentication.CheckedAt)
	}
	if view.Authentication.Context != "" {
		checked += "; " + view.Authentication.Context
	}
	terminal.Printf(out, "  Authentication: %s (%s)\n", view.Authentication.State, checked)
	terminal.Printf(out, "  Background:    %s\n", view.Background)
	if view.Paused {
		terminal.Println(out, "  Collection:    paused")
	}
	terminal.Printf(out, "  Projects:      %d included\n  Pending:       %d session(s)\n  Last scan:     %s\n  Last publish:  %s\n", len(view.Projects), view.Collector.PendingCount, formatTimeOrNever(view.Collector.LastScanAt), formatTimeOrNever(view.Collector.LastPublishedAt))
	terminal.Printf(out, "  Skill evidence: %s\n", view.SkillEvidence)
	if view.ImportedSessions > 0 {
		imported := fmt.Sprintf("%d session(s), %d waiting to upload", view.ImportedSessions, view.ImportedPending)
		if view.ImportedWithIssues > 0 {
			imported += fmt.Sprintf(", %d with a capture gap or failed scan", view.ImportedWithIssues)
		}
		if view.LastImport != "" {
			imported += "; last import " + view.LastImport
		}
		terminal.Printf(out, "  Imported:      %s\n", imported)
	}
	for _, app := range view.Apps {
		printAppDetails(out, app)
	}
	for _, diagnostic := range view.CaptureDiagnostics {
		terminal.Printf(out, "  Capture skipped in %s (%s): %s at %s.\n", diagnostic.ProjectRoot, appName(diagnostic.Harness), capture.DiagnosticMessage(diagnostic.Code), formatTimeOrNever(diagnostic.ObservedAt))
	}
	if view.Collector.LastError != "" {
		terminal.Printf(out, "  Last error:    %s\n", view.Collector.LastError)
	}
	if n := len(view.Collector.QuarantinedFiles); n > 0 {
		terminal.Printf(out, "  Quarantined:   %d local state file(s) could not be read and were moved aside; their sessions keep their other evidence. See status --json for the files, then delete them.\n", n)
	}
	if n := view.Collector.UnrefreshableSummaries; n > 0 {
		terminal.Printf(out, "  Summaries:     %d session summary(ies) cannot be refreshed by this version and stay as published until the session changes.\n", n)
	}
	for _, warning := range view.Warnings {
		terminal.Printf(out, "  Warning:       %s\n", warning)
	}
}

// printAppDetails writes one app's lines in the Details section.
func printAppDetails(out io.Writer, app appStatus) {
	gaps := ""
	if len(app.CaptureGaps) > 0 {
		gaps = fmt.Sprintf("; %d with a capture gap", app.SessionsWithCaptureGaps)
	}
	terminal.Printf(out, "  %s: %s (%s; %d session(s)%s); hooks %s\n", appName(app.Name), app.State, app.Code, app.Sessions, gaps, app.Hooks)
	if app.Trust == "unknown" {
		terminal.Println(out, "    Hook trust: unknown here; it is granted inside the app and is not observable from this Mac's files.")
	}
	terminal.Printf(out, "    Installed version: %s; support %s%s.\n", installedVersionLabel(app), app.VersionSupport, versionSupportNote(app))
	if app.Capabilities.FreshStart.State == capabilityUnavailable {
		terminal.Printf(out, "    Fresh-start capture: unavailable. %s\n", app.Capabilities.FreshStart.NextAction)
	}
	for _, pair := range app.Projects {
		terminal.Printf(out, "    Project %s: %s.\n", pair.ProjectRoot, pair.VerificationState)
	}
	switch {
	case app.ReadBackVerified && !app.VerifiedAt.IsZero():
		terminal.Printf(out, "    Read-back verified: %s; evidence is for that publication.\n", formatTimeOrNever(app.VerifiedAt))
	case !app.VerifiedAt.IsZero():
		// Some evidence exists but not every project (or session) is
		// covered, so do not call the app verified.
		terminal.Printf(out, "    Last read-back: %s (%s).\n", formatTimeOrNever(app.VerifiedAt), readBackProgress(app))
	}
	if app.VerificationDetail != "" {
		terminal.Printf(out, "    Read-back: %s\n", app.VerificationDetail)
	}
	if len(app.CaptureGaps) > 0 {
		terminal.Printf(out, "    Capture gaps: %d recorded across %d session(s); see status --json for details.\n", len(app.CaptureGaps), app.SessionsWithCaptureGaps)
	}
}

// storageAccessLine is the Details section's Access line: when access to
// the destination was last confirmed, and by what.
func storageAccessLine(view statusView) string {
	switch view.StorageAccessConfirmedBy {
	case storageAccessConfirmedBySetup:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by setup's storage check (write, read, list, delete)"
	case storageAccessConfirmedByCollector:
		return "confirmed " + formatTimeOrNever(view.StorageAccessConfirmedAt) + " by the collector's last successful storage access"
	}
	return "not confirmed yet"
}

// printBucketPrivacy writes the Details section's bucket privacy lines: the
// last inspection's result, when it ran, its reason code, and the
// provider's guidance.
func printBucketPrivacy(out io.Writer, report storage.PrivacyReport) {
	//lint:ignore LV1001 storage.PrivacyReport.State is an untyped string owned by package storage
	switch report.State {
	case "verified_private":
		terminal.Println(out, "  Bucket privacy: native public access blocked at the last check.")
	case "public_or_risky":
		terminal.Println(out, "  Bucket privacy: public configuration detected; review access before archiving.")
	default:
		terminal.Println(out, "  Bucket privacy not verified.")
	}
	checked := "never"
	if report.CheckedAt != nil {
		checked = formatTimeOrNever(*report.CheckedAt)
	}
	terminal.Printf(out, "    Checked: %s; %s.\n    Review: %s\n", checked, report.Reason, report.GuidanceURL)
}

// versionSupportNote explains an unverified installed version without
// changing the support state or reason code.
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

// installedVersionLabel is an app's installed version, or why it is not
// known.
func installedVersionLabel(app appStatus) string {
	if app.InstalledVersion != "" {
		return app.InstalledVersion
	}
	if app.VersionState != "" {
		return app.VersionState
	}
	return "unknown"
}
