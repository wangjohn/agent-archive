package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const defaultPrefix = "agent-archive/"

const defaultRetentionDays = 90

var allHarnesses = []string{"codex", "claude", "cursor"}

type setupDraft struct {
	StagedRefs    []string      `json:"staged_credential_refs,omitempty"`
	Version       int           `json:"version"`
	Config        config.Config `json:"config"`
	Step          int           `json:"step"`
	CredentialRef string        `json:"staged_credential_ref,omitempty"`
	// StopImported lists imported-only apps whose imports the person chose to
	// stop publishing. Config.ImportedHarnesses itself always comes from the
	// committed configuration (see carriedImportedHarnesses).
	StopImported []string `json:"stop_imported,omitempty"`
}

// draftFormat is the setupDraft.Version this release writes and reads.
const draftFormat = 1

func draftPath(home string) string { return filepath.Join(home, "setup-draft.json") }

// readDraft reads the saved setup. found is whether one exists; problem says
// why one that exists cannot be used (it does not decode, or another version
// of agent-archive wrote it), and err is any other failure to read it.
func readDraft(home string) (draft setupDraft, found bool, problem string, err error) {
	err = local.Read(draftPath(home), &draft)
	switch {
	case os.IsNotExist(err):
		return setupDraft{}, false, "", nil
	case state.IsUndecodable(err):
		return setupDraft{}, true, fmt.Sprintf("it does not read as a saved setup: %v", err), nil
	case err != nil:
		return setupDraft{}, true, "", err
	case draft.Version > draftFormat:
		return setupDraft{}, true, fmt.Sprintf("a newer version of agent-archive saved it (format %d)", draft.Version), nil
	case draft.Version != draftFormat || draft.Step < 0 || draft.Step > 2:
		return setupDraft{}, true, "it is not in a format this version saves", nil
	}
	return draft, true, "", nil
}

// offerUnusableDraft reads the saved setup, and when one exists but cannot
// be used, names it and offers to move it aside so setup can go on without
// it (declining leaves it, and stops). have is whether a usable draft was
// read.
func offerUnusableDraft(p *prompter, home string) (saved setupDraft, have bool, err error) {
	saved, found, problem, err := readDraft(home)
	if err != nil {
		return setupDraft{}, false, fmt.Errorf("read the saved setup %s: %w", draftPath(home), err)
	}
	if problem == "" {
		return saved, found, nil
	}
	p.warn(fmt.Sprintf("The saved setup in %s cannot be used: %s.", draftPath(home), problem),
		"Moving it aside keeps it, renamed, for reference, and setup starts again from your current settings.",
		"A Keychain item it staged, if any, stays in the Keychain (service "+credentials.KeychainService+").")
	move, err := p.yesNo("Move it aside and continue?", true)
	if err != nil {
		return setupDraft{}, false, err
	}
	if !move {
		return setupDraft{}, false, fmt.Errorf("the saved setup in %s cannot be used (%s); move it aside or delete it, then run agent-archive setup", draftPath(home), problem)
	}
	aside, err := moveAside(draftPath(home))
	if err != nil {
		return setupDraft{}, false, err
	}
	p.note("Moved it to " + aside + ".")
	return setupDraft{}, false, nil
}

func runSetupCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("setup", stderr)
	abandon := fs.Bool("abandon-recovery", false, "keep every file as it is now and discard an interrupted setup")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if *abandon {
		if err := abandonRecovery(stdout, env); err != nil {
			terminal.Printf(stderr, "agent-archive: setup: %v\n", err)
			return 1
		}
		return 0
	}
	// Every step asks something, so without a terminal setup would stop at
	// its first question with nothing but an end-of-input error.
	if !env.isTerminal(stdin) {
		terminal.Println(stderr, "agent-archive: setup: setup asks questions and needs a terminal. Nothing was changed. Run agent-archive setup in Terminal.")
		return 1
	}
	if err := setup(stdin, stdout, stderr, env); err != nil {
		terminal.Printf(stderr, "Setup incomplete: %v\n", err)
		var blocked *setupjournal.RecoveryBlockedError
		if errors.As(err, &blocked) {
			terminal.Println(stderr, blocked.Guidance())
			return 1
		}
		var other *otherInstallationError
		if errors.As(err, &other) {
			terminal.Println(stderr, other.guidance())
			return 1
		}
		terminal.Println(stderr, "Run agent-archive setup to continue.")
		return 1
	}
	return 0
}

func setup(stdin io.Reader, out, errOut io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	// Only another wizard is excluded while prompting; the old collector keeps working.
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	if err = recoverSetup(home, env); err != nil {
		return err
	}
	existing, found, err := config.Load(home)
	if err != nil {
		return err
	}
	terminal.Println(out, "Checking installed applications...")
	discoveries := env.discoverApplications(userHome)
	discoveredAt := env.now()
	// The review shows these; discoveries themselves are recorded unchanged.
	reviewed := reviewDiscoveries(discoveries, env.detectHarnesses(userHome))
	p := newPrompter(stdin, out)
	p.now = env.now
	// Said before any question: setup will refuse to install an app's hooks
	// beside another installation's (see applySetup).
	for _, problem := range env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), allHarnesses) {
		p.warn(problem)
	}
	if !found {
		terminal.Println(out, "You’ll need a private Cloudflare R2 or Amazon S3 bucket. Setup instructions are available when you choose storage.")
	}
	draft := setupDraft{Version: draftFormat, Config: existing}
	savedPath := draftPath(home)
	saved, haveDraft, err := offerUnusableDraft(p, home)
	if err != nil {
		return err
	}
	if haveDraft {
		choice, e := p.menu("You have an unfinished setup. What would you like to do?", "continue",
			option{"continue", "Continue where you left off"},
			option{"capture", "Change apps and projects"},
			option{"storage", "Change storage"},
			option{"retention", "Change how long sessions are kept"},
			option{"restart", "Start over"})
		if e != nil {
			return e
		}
		if choice != "restart" {
			draft = saved
			// Projects an import added after this draft was saved are kept:
			// the draft never saw them, so it cannot have meant to drop them.
			draft.Config.Archive.Projects = withBackfilledProjects(draft.Config.Archive.Projects, existing.Archive.Projects, backfilledProjects(env))
			if choice == "storage" {
				draft.Step = 1
			}
			if choice == "capture" {
				if e = chooseCapture(p, &draft.Config, userHome, env); e != nil {
					return e
				}
				if e = offerStopImported(p, &draft, existing); e != nil {
					return e
				}
				draft.Step = 2
				if draft.Config.Storage.Provider == "" {
					draft.Step = 1
				}
			}
			if choice == "retention" {
				days := draft.Config.RetentionDays
				if days <= 0 {
					days = defaultRetentionDays
				}
				draft.Config.RetentionDays, e = p.retentionDays(days)
				if e != nil {
					return e
				}
			}
		} else {
			if e = discardDraft(home, saved, existing, env); e != nil {
				return e
			}
		}
	} else if found {
		choice, e := p.menu("Agent Archive is already set up. What would you like to change?", "capture",
			option{"capture", "Apps and projects"},
			option{"storage", "Storage (bucket and credentials)"},
			option{"retention", "How long sessions are kept"},
			option{"all", "All settings"})
		if e != nil {
			return e
		}
		//lint:ignore LV1001 menu keys are the option keys listed just above
		switch choice {
		case "storage":
			draft.Step = 1
		case "retention":
			draft.Step = 2
			draft.Config.RetentionDays, err = p.retentionDays(existing.RetentionDays)
			if err != nil {
				return err
			}
		}
		if choice == "capture" { // Storage is still verified, but its prompts are skipped.
			if err = chooseCapture(p, &draft.Config, userHome, env); err != nil {
				return err
			}
			if err = offerStopImported(p, &draft, existing); err != nil {
				return err
			}
			draft.Step = 2
		}
	}
	save := func() error { return local.Write(savedPath, draft) }
	var verifiedStorage credentials.Config
	for {
		if draft.Step == 0 {
			if err = chooseCapture(p, &draft.Config, userHome, env); err != nil {
				return err
			}
			draft.Step = 1
			if err = save(); err != nil {
				return err
			}
		}
		// The apps are chosen now, so whether applySetup would refuse them
		// beside another installation's hooks is known: say so before the
		// storage and retention questions, not after them. The answers so
		// far are saved for the next run.
		if problems := env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), draft.Config.Harnesses); len(problems) > 0 {
			if err = save(); err != nil {
				return err
			}
			return &otherInstallationError{problems: problems}
		}
		if draft.Step == 1 {
			p.step(2, "Connect storage")
			cfg, secret, saveSecret, e := promptStorage(p, draft.Config.Storage, env)
			if e != nil {
				return e
			}
			if saveSecret {
				keychain, e := env.keychain()
				if e != nil {
					return fmt.Errorf("open Keychain: %w", e)
				}
				id, e := local.ID()
				if e != nil {
					return e
				}
				cfg.R2CredentialRef = "setup-" + id
				draft.CredentialRef = cfg.R2CredentialRef
				draft.StagedRefs = append(draft.StagedRefs, cfg.R2CredentialRef)
				// Journal the opaque reference before storing, so cancellation/crash is recoverable.
				draft.Config.Storage = cfg
				if e = save(); e != nil {
					return e
				}
				if e = keychain.Save(context.Background(), cfg.R2CredentialRef, secret); e != nil {
					return fmt.Errorf("save staged credential: %w", e)
				}

			}
			draft.Config.Storage = cfg
			draft.Step = 2
			if err = save(); err != nil {
				return err
			}
		}
		if err = save(); err != nil {
			return err
		}
		if draft.Config.Storage.Provider == credentials.ProviderR2 {
			kc, e := env.keychain()
			if e != nil {
				return e
			}
			if _, e = kc.Load(context.Background(), draft.Config.Storage.R2CredentialRef); e != nil {
				draft.Step = 1
				draft.Config.Storage.R2CredentialRef = ""
				_ = save()
				return fmt.Errorf("stored R2 credential is unavailable; enter it again during setup")
			}
		}
		if draft.Config.Storage != verifiedStorage {
			terminal.Println(out, "\nChecking your storage connection…")
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			store, e := env.openStore(draft.Config)
			if e != nil {
				cancel()
				draft.Step = 1
				_ = save()
				return fmt.Errorf("connect storage: %w", e)
			}
			e = storage.VerifyAccess(ctx, store)
			cancel()
			if e != nil {
				failure := fmt.Errorf("storage test failed: %w (check access and retry; saved choices are kept)", e)
				terminal.Println(out, failure)
				choice, promptErr := p.menu("What would you like to do?", "cancel",
					option{"edit", "Edit settings"},
					option{"retry", "Retry the storage check"},
					option{"cancel", "Cancel (your choices are kept)"})
				if promptErr != nil || choice == "cancel" {
					return failure
				}
				if choice == "edit" {
					if err = editSetupReview(p, &draft, userHome, backfilledProjects(env)); err != nil {
						return err
					}
					if err = save(); err != nil {
						return err
					}
				}
				continue
			}
			draft.Config.BucketPrivacy = inspectBucketPrivacy(draft.Config, store, env.now())
			draft.Config.StorageVerifiedAt = env.now().UTC()
			verifiedStorage = draft.Config.Storage
			terminal.Println(out, p.style.green("✓ Connected."))
		}

		if draft.Config.RetentionDays <= 0 {
			draft.Config.RetentionDays = defaultRetentionDays
		}
		// Review what will be committed, not what a draft may have saved.
		draft.Config.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, draft.Config.Harnesses, draft.StopImported)
		showSetupReview(p, draft.Config, existing, found, reviewed)
		terminal.Println(out, "\n"+p.style.bold("Before you confirm"))
		if err = reviewChanges(home, existing, draft.Config, p, env); err != nil {
			return err
		}
		if existing.Paused {
			p.note("Capture stays paused until you run agent-archive resume.")
		}
		reviewHookFiles(p, draft.Config.Harnesses, env.hookFiles(userHome), env.installedHookFiles(userHome, existing), existing.Harnesses, len(existing.HookFiles) > 0)
		printReviewNotes(p, draft.Config, reviewed)
		action, e := reviewAction(p, found)
		if e != nil {
			return e
		}
		if action == "cancel" {
			terminal.Println(out, "Cancelled. Active settings are unchanged; your setup draft is saved.")
			return nil
		}
		if action == "edit" {
			if err = editSetupReview(p, &draft, userHome, backfilledProjects(env)); err != nil {
				return err
			}
			if err = save(); err != nil {
				return err
			}
			continue
		}

		for _, ref := range draft.StagedRefs {
			if ref != draft.Config.Storage.R2CredentialRef && !containsString(draft.Config.RetiredCredentialRefs, ref) {
				draft.Config.RetiredCredentialRefs = append(draft.Config.RetiredCredentialRefs, ref)
			}
		}
		// Re-read under the machine lock in applySetup; it rejects concurrent config changes.
		if err = applySetup(home, userHome, exe, existing, &draft.Config, draft.StopImported, env); err != nil {
			return err
		}
		if err = recordApplicationDiscoveries(home, discoveries, discoveredAt); err != nil {
			terminal.Printf(out, "Warning: installed application versions could not be recorded: %v\n", err)
		}
		if err = os.Remove(savedPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		// The configuration is committed; a diagnostic for a project that
		// was just excluded is stale local state, not a reason to fail.
		if e := capture.PruneDiagnostics(home, draft.Config.Archive.Projects); e != nil {
			terminal.Printf(errOut, "Could not prune capture diagnostics for excluded projects: %v\n", e)
		}
		terminal.Println(out, "\nConfiguration saved.")
		if existing.Paused {
			terminal.Println(out, "Next: run agent-archive resume when you’re ready to start archiving.")
		} else if containsString(draft.Config.Harnesses, "codex") {
			terminal.Println(out, "Next: in Codex CLI, open /hooks to approve the archive hooks, then start a new session in an included project.")
			if len(draft.Config.Harnesses) > 1 {
				terminal.Println(out, "Repeat hook approval and a new session in your other selected apps.")
			}
		} else {
			terminal.Println(out, "Next: approve the archive hooks in your selected apps, then start a new session in an included project.")
		}
		terminal.Println(out, "Check progress with agent-archive status.")
		return nil
	}
}

func chooseCapture(p *prompter, cfg *config.Config, userHome string, env Env) error {
	p.step(1, "Choose what to capture")
	err := chooseHarnesses(p, env.detectHarnesses(userHome), cfg)
	if err != nil {
		return err
	}
	if len(cfg.Harnesses) == 0 {
		return fmt.Errorf("choose at least one application")
	}
	acceptedProject := false
	if len(cfg.Archive.Projects) == 0 {
		dir, e := os.Getwd()
		if env.WorkingDir != nil {
			dir, e = env.WorkingDir()
		}
		if e == nil {
			if root := suggestedProject(dir); root != "" {
				terminal.Printf(p.out, "Project: %s\n", root)
				acceptedProject, err = p.yesNo("Archive sessions in this project?", true)
				if err != nil {
					return err
				}
				if acceptedProject {
					cfg.Archive.Projects = []archive.ProjectActivation{{ProjectID: archive.ProjectID(root), Root: root, Included: true}}
				}
			}
		}
	}
	if !acceptedProject {
		cfg.Archive.Projects, err = promptProjects(p, cfg.Archive.Projects, backfilledProjects(env), userHome)
		if err != nil {
			return err
		}
	}

	if includedProjects(cfg.Archive.Projects) == 0 {
		return fmt.Errorf("choose at least one project")
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	return nil
}

func promptStorage(p *prompter, existing credentials.Config, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	cfg := existing
	var secret credentials.R2Credentials
	providers := []option{
		{"r2", "Cloudflare R2"},
		{"s3", "Amazon S3"},
		{"help", "Show setup instructions"},
	}
	choice, err := p.menu("Where should sessions be stored?", firstNonEmpty(existing.Provider, "r2"), providers...)
	for err == nil && choice == "help" {
		terminal.Println(p.out, "Cloudflare R2: create a private bucket and bucket-scoped Object Read & Write credentials. Keep public access disabled.")
		terminal.Println(p.out, "https://developers.cloudflare.com/r2/get-started/s3/")
		terminal.Println(p.out, "Amazon S3: create a private bucket and configure an AWS profile with access to it.")
		terminal.Println(p.out, "https://docs.aws.amazon.com/AmazonS3/latest/userguide/create-bucket-overview.html")
		terminal.Println(p.out, "https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html")
		choice, err = p.menu("Where should sessions be stored?", firstNonEmpty(existing.Provider, "r2"), providers[:2]...)
	}
	if err != nil {
		return cfg, secret, false, err
	}
	if cfg.Provider != choice {
		cfg = credentials.Config{Provider: choice}
	}
	cfg.Bucket, err = p.required("Bucket name", cfg.Bucket)
	if err != nil {
		return cfg, secret, false, err
	}
	if choice == "r2" {
		for {
			endpoint, e := p.required("R2 account ID or full S3 endpoint", firstNonEmpty(cfg.R2Endpoint, cfg.R2AccountID))
			if e != nil {
				return cfg, secret, false, e
			}
			if strings.Contains(endpoint, "://") {
				cfg.R2Endpoint = endpoint
				cfg.R2AccountID = ""
			} else {
				cfg.R2AccountID = endpoint
				cfg.R2Endpoint = ""
			}
			normalized, e := credentials.R2Endpoint(cfg.R2Endpoint, cfg.R2AccountID)
			if e == nil {
				cfg.R2Endpoint = normalized
				break
			}
			terminal.Println(p.out, "Enter the Cloudflare R2 S3 endpoint or account ID from your dashboard.")
		}
		reuse := false
		if cfg.R2CredentialRef != "" {
			reuse, err = p.yesNo("Keep stored R2 credentials?", true)
			if err != nil {
				return cfg, secret, false, err
			}
		}
		if !reuse {
			secret.AccessKeyID, err = p.required("Access key ID", "")
			if err != nil {
				return cfg, secret, false, err
			}
			for secret.SecretAccessKey == "" {
				secret.SecretAccessKey, err = p.secret("Secret access key (hidden): ")
				if err != nil {
					return cfg, secret, false, err
				}
			}
		}
	} else {
		if err = promptAWSProfile(p, &cfg, env); err != nil {
			return cfg, secret, false, err
		}
	}
	cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)

	return cfg, secret, secret.SecretAccessKey != "", err
}

// chooseHarnesses asks which apps to include and keeps cfg.DeclinedHarnesses
// in step. On reconfiguration, an app the user leaves out after it was
// offered as found, or removes from the saved selection, is remembered so
// later runs do not offer it again (detection only sees a config directory,
// which stays after an app is excluded on purpose). An app that ends up
// included is no longer declined.
func chooseHarnesses(p *prompter, detected []string, cfg *config.Config) error {
	previous := cfg.Harnesses
	var offered, found []string
	for _, app := range detected {
		if containsString(cfg.DeclinedHarnesses, app) {
			continue
		}
		offered = append(offered, app)
		if len(cfg.Harnesses) > 0 && !containsString(cfg.Harnesses, app) {
			found = append(found, app)
		}
	}
	harnesses, err := promptHarnesses(p, offered, cfg.Harnesses)
	if err != nil {
		return err
	}
	var declined []string
	for _, app := range allHarnesses {
		if !containsString(harnesses, app) && (containsString(cfg.DeclinedHarnesses, app) || containsString(found, app) || containsString(previous, app)) {
			declined = append(declined, app)
		}
	}
	cfg.Harnesses, cfg.DeclinedHarnesses = harnesses, declined
	return nil
}

func promptHarnesses(p *prompter, detected, existing []string) ([]string, error) {
	// Preserve an existing selection on reconfiguration. Detection supplies
	// defaults for first-time setup and, on reconfiguration, offers apps the
	// selection leaves out; it never proves capture is working.
	defaults := detected
	if len(existing) > 0 {
		defaults = existing
	}
	var suggested, others, found []string
	for _, app := range allHarnesses {
		switch {
		case containsString(defaults, app):
			suggested = append(suggested, app)
		case containsString(detected, app):
			// Only reachable on reconfiguration: detected but not included.
			found = append(found, app)
			others = append(others, app)
		default:
			others = append(others, app)
		}
	}
	switch {
	case len(suggested) == 0:
		terminal.Println(p.out, "No apps found automatically.")
	case len(existing) > 0 && len(others) > 0:
		if len(found) > 0 {
			// Detected apps the saved selection leaves out are offered on
			// their own, defaulting to yes. Declining still allows other
			// changes below.
			terminal.Printf(p.out, "Included: %s.\nAlso found on this computer: %s.\n", appList(suggested), appList(found))
			add, err := p.yesNo(addPrompt(found), true)
			if err != nil {
				return nil, err
			}
			if add {
				var result []string
				for _, app := range allHarnesses {
					if containsString(suggested, app) || containsString(found, app) {
						result = append(result, app)
					}
				}
				return result, nil
			}
		} else {
			// Name the apps left out so it is clear how to add them; "Keep X?"
			// reads as if declining would remove X.
			terminal.Printf(p.out, "Included: %s. Not included: %s.\n", appList(suggested), appList(others))
		}
		change, err := p.yesNo("Change which apps are included?", false)
		if err != nil {
			return nil, err
		}
		if !change {
			return suggested, nil
		}
	default:
		verb := "Include "
		if len(existing) > 0 {
			verb = "Keep "
		}
		yes, err := p.yesNo(verb+appList(suggested)+"?", true)
		if err != nil {
			return nil, err
		}
		if yes {
			return suggested, nil
		}
	}
	terminal.Println(p.out, "Choose which apps to include:")
	for {
		var result []string
		for _, app := range allHarnesses {
			yes, err := p.yesNo("Include "+appName(app)+"?", containsString(suggested, app))
			if err != nil {
				return nil, err
			}
			if yes {
				result = append(result, app)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
		terminal.Println(p.out, "Choose at least one app to continue.")
	}
}

// addPrompt asks to add newly found apps: "Add it?" or "Add them?".
func addPrompt(apps []string) string {
	if len(apps) == 1 {
		return "Add it?"
	}
	return "Add them?"
}

// appList names apps in prose: "Codex", "Codex and Cursor", or
// "Codex, Claude Code, and Cursor".
func appList(apps []string) string {
	names := make([]string, len(apps))
	for i, app := range apps {
		names[i] = appName(app)
	}
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

// promptProjects asks which included projects to keep, then for new ones.
// Projects backfill added (backfilled holds their project IDs) are kept or
// excluded together with one question, since an import can add hundreds.
// An excluded project stays in the list as excluded, so its exclusion keeps
// holding: its imported sessions stop uploading and later backfills skip it.
// A project that was not imported and is not kept is dropped, as before.
func promptProjects(p *prompter, existing []archive.ProjectActivation, backfilled map[string]bool, userHomes ...string) ([]archive.ProjectActivation, error) {
	result := []archive.ProjectActivation{}
	seen := map[string]bool{}
	imported := 0
	for _, project := range existing {
		if project.Included && backfilled[project.ProjectID] {
			imported++
		}
	}
	keepImported := true
	if imported > 1 {
		var err error
		keepImported, err = p.yesNo(fmt.Sprintf("Keep the %d projects added by backfill? If not, their imported sessions stop uploading and later backfills skip them.", imported), true)
		if err != nil {
			return nil, err
		}
	}
	for _, project := range existing {
		if !project.Included {
			// Exclusions, including those undo leaves, carry through.
			result = append(result, project)
			continue
		}
		keep := keepImported
		if imported <= 1 || !backfilled[project.ProjectID] {
			var err error
			if keep, err = p.yesNo("Keep project "+project.Root+"?", true); err != nil {
				return nil, err
			}
		}
		switch {
		case keep:
			result = append(result, project)
			seen[project.Root] = true
		case backfilled[project.ProjectID]:
			project.Included = false
			result = append(result, project)
		}
	}
	terminal.Println(p.out, "Add project directories, one per line. Enter a blank line when finished.")
	for {
		root, err := p.line("Project path: ")
		if err != nil {
			return nil, err
		}
		if root == "" {
			break
		}
		if root == "~" || strings.HasPrefix(root, "~/") {
			home, e := os.UserHomeDir()
			if len(userHomes) > 0 {
				home = userHomes[0]
				e = nil
			}
			if e != nil {
				return nil, e
			}
			root = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(root, "~"), "/"))
		}
		root, err = filepath.Abs(root)
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}
		if err != nil {
			terminal.Println(p.out, "That directory does not exist. Enter an existing project path.")
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			terminal.Println(p.out, "Enter a directory, not a file.")
			continue
		}
		if seen[root] {
			terminal.Println(p.out, "That project is already included.")
			continue
		}
		seen[root] = true
		reincluded := false
		for i := range result {
			if result[i].Root == root {
				result[i].Included, reincluded = true, true
			}
		}
		if reincluded {
			continue
		}
		// ActivatedAt is left zero here; setup stamps it when it commits.
		project := archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true}
		for _, old := range existing {
			if old.Root == root {
				project.ActivatedAt = old.ActivatedAt
			}
		}
		result = append(result, project)
	}
	return result, nil
}

// includedProjects counts the projects capture is on for.
func includedProjects(projects []archive.ProjectActivation) int {
	n := 0
	for _, project := range projects {
		if project.Included {
			n++
		}
	}
	return n
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func appName(app string) string {
	//lint:ignore LV1001 harness names are plain strings in config and archive; an unknown name is shown as given
	switch app {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	case "cursor":
		return "Cursor"
	}
	return app
}

func friendlyApps(apps []string) string {
	if len(apps) == 0 {
		return "none"
	}
	names := []string{}
	for _, a := range apps {
		names = append(names, appName(a))
	}
	return strings.Join(names, ", ")
}

func suggestedProject(dir string) string {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}

// withBackfilledProjects adds to a resumed draft's projects the committed
// projects an import added that the draft does not mention.
func withBackfilledProjects(draft, committed []archive.ProjectActivation, backfilled map[string]bool) []archive.ProjectActivation {
	mentioned := map[string]bool{}
	for _, project := range draft {
		mentioned[project.Root] = true
	}
	for _, project := range committed {
		if backfilled[project.ProjectID] && !mentioned[project.Root] {
			draft = append(draft, project)
		}
	}
	return draft
}

// backfilledProjects is the set of project IDs any backfill import added:
// the projects it imported into, and the folders inside an added plain
// folder it added excluded (ProjectsKeptOut). A kept-out entry must never
// be dropped while the folder around it is included: that folder would
// then capture it. So a resumed draft carries it (withBackfilledProjects),
// and declining it in setup excludes it rather than removing it. An
// unreadable batch file leaves its projects out, so they are asked about
// one by one, as before imports existed.
func backfilledProjects(env Env) map[string]bool {
	out := map[string]bool{}
	home, err := env.home()
	if err != nil {
		return out
	}
	batches, _ := backfill.LoadBatches(home)
	for _, b := range batches {
		for _, id := range slices.Concat(b.ProjectsAdded, b.ProjectsKeptOut) {
			out[id] = true
		}
	}
	return out
}
