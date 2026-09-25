package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// The environment variables setup --yes reads R2 keys from. The secret is
// never taken from a command argument, which other processes can read.
const (
	envR2AccessKeyID     = "AGENT_ARCHIVE_R2_ACCESS_KEY_ID"
	envR2SecretAccessKey = "AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY" //nolint:gosec // G101: a variable's name, not a credential.
)

// setupOptions are setup's answers given as flags, for setup --yes.
type setupOptions struct {
	provider             string
	bucket               string
	r2Account            string
	r2KeyID              string
	awsProfile           string
	region               string
	apps                 string
	projects             []string
	yes                  bool
	storageFlagsSupplied bool
}

// projectList is a repeatable --project.
type projectList []string

func (l *projectList) String() string { return strings.Join(*l, ",") }

func (l *projectList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// setupFlags adds setup's answer flags to fs and parses args.
func setupFlags(fs *commandFlags, args []string) (setupOptions, bool) {
	var opts setupOptions
	var projects projectList
	fs.StringVar(&opts.provider, "provider", "", "storage provider: r2 or s3")
	fs.StringVar(&opts.bucket, "bucket", "", "bucket name")
	fs.StringVar(&opts.r2Account, "r2-account", "", "R2 account ID, or the bucket URL")
	fs.StringVar(&opts.r2KeyID, "r2-access-key-id", "", "R2 access key ID")
	fs.StringVar(&opts.awsProfile, "aws-profile", "", "AWS profile for S3")
	fs.StringVar(&opts.region, "region", "", "S3 bucket region")
	fs.StringVar(&opts.apps, "apps", "", "apps to capture, comma-separated")
	fs.Var(&projects, "project", "project directory to capture (repeatable)")
	fs.BoolVar(&opts.yes, "yes", false, "apply without questions")
	if !fs.parseFlagsOnly(args) {
		return opts, false
	}
	opts.projects = projects
	fs.Visit(func(f *flag.Flag) {
		//lint:ignore LV1001 flag names are the ones defined just above
		switch f.Name {
		case "provider", "bucket", "r2-account", "r2-access-key-id", "aws-profile", "region":
			opts.storageFlagsSupplied = true
		}
	})
	return opts, true
}

// given reports whether any answer flag was passed.
func (o setupOptions) given() bool {
	return o.storageFlagsSupplied || o.apps != "" || len(o.projects) > 0
}

// setupWithoutQuestions is setup --yes: the answers come from opts, the
// saved configuration, and what setup detects. It runs the same storage
// check and the same transaction as interactive setup, and refuses before
// changing anything when an answer is missing.
func setupWithoutQuestions(opts setupOptions, stdin io.Reader, out, errOut io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
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
	existing, _, err := config.Load(home)
	if err != nil {
		return err
	}
	if _, saved, _, e := readDraft(home); e != nil || saved {
		return fmt.Errorf("an unfinished setup is saved in %s; run agent-archive setup to finish or discard it first", draftPath(home))
	}
	cfg := existing
	cfg.Archive.Projects = slices.Clone(existing.Archive.Projects)
	if err = setupApps(&cfg, opts.apps, env.detectHarnesses(userHome)); err != nil {
		return err
	}
	if problems := env.installation(home, userHome).otherInstallationProblems(env.hookFiles(userHome), cfg.Harnesses); len(problems) > 0 {
		return &otherInstallationError{problems: problems}
	}
	if err = setupProjects(&cfg, opts.projects, userHome); err != nil {
		return err
	}
	secret, err := setupStorageFromFlags(&cfg, opts, env)
	if err != nil {
		return err
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	p := newPrompter(stdin, out)
	p.now = env.now
	if cfg.Storage.Provider == credentials.ProviderR2 && secret.AccessKeyID != "" {
		if secret.SecretAccessKey, err = readR2Secret(p, stdin, env); err != nil {
			return err
		}
	}
	discoveries := env.discoverApplications(userHome)
	discoveredAt := env.now()

	// A new R2 key is staged under a draft, as interactive setup stages it,
	// so an interrupted run leaves a setup to finish or discard, and a
	// failed one removes the key again.
	draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2}
	discard := func(failure error) error {
		if e := discardDraft(home, draft, existing, env); e != nil {
			return fmt.Errorf("%w (and the staged R2 key could not be removed: %w)", failure, e)
		}
		return failure
	}
	if secret.SecretAccessKey != "" {
		if err = stageR2Key(home, &cfg, &draft, secret, env); err != nil {
			return discard(err)
		}
	} else if cfg.Storage.Provider == credentials.ProviderR2 && !storedCredentialReadable(env, cfg.Storage.R2CredentialRef) {
		return fmt.Errorf("the stored R2 key can't be read from the Keychain; pass --r2-access-key-id and the secret (see agent-archive setup --help)")
	}

	terminal.Println(out, "Checking your storage connection…")
	connectErr, accessErr := verifyStorage(&cfg, env)
	if connectErr != nil {
		return discard(connectErr)
	}
	if accessErr != nil {
		return discard(fmt.Errorf("storage test failed: %w; nothing was changed", accessErr))
	}
	terminal.Println(out, p.style.green("✓ Connected."))
	cfg.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, cfg.Harnesses, nil)
	// A reconfiguration's warnings, or its refusal (sessions pending at the
	// old destination).
	if err = reviewChanges(home, existing, cfg, p, env); err != nil {
		return discard(err)
	}
	terminal.Printf(out, "Apps: %s. Projects: %d. Storage: %s bucket %s.\n", appList(cfg.Harnesses), includedProjects(cfg.Archive.Projects), providerName(cfg.Storage.Provider), cfg.Storage.Bucket)
	printReviewPrivacy(p, cfg)
	if err = applySetup(home, userHome, exe, existing, &cfg, nil, env); err != nil {
		return discard(err)
	}
	return finishSetup(p, errOut, home, cfg, existing.Paused, discoveries, discoveredAt)
}

// stageR2Key saves a new R2 key in the Keychain under a fresh reference,
// which it sets in cfg. The reference is written to draft's file first, so
// a run stopped in between leaves a setup that can be discarded.
func stageR2Key(home string, cfg *config.Config, draft *setupDraft, secret credentials.R2Credentials, env Env) error {
	keychain, err := env.keychain()
	if err != nil {
		return fmt.Errorf("open Keychain: %w", err)
	}
	id, err := local.ID()
	if err != nil {
		return err
	}
	cfg.Storage.R2CredentialRef = "setup-" + id
	draft.Config.Storage = cfg.Storage
	draft.CredentialRef, draft.StagedRefs = cfg.Storage.R2CredentialRef, []string{cfg.Storage.R2CredentialRef}
	if err = local.Write(draftPath(home), draft); err != nil {
		return err
	}
	if err = keychain.Save(context.Background(), cfg.Storage.R2CredentialRef, secret); err != nil {
		return fmt.Errorf("save the R2 key: %w", err)
	}
	return nil
}

// providerName is how setup names a storage provider.
func providerName(provider string) string {
	if provider == credentials.ProviderS3 {
		return "Amazon S3"
	}
	return "Cloudflare R2"
}

// setupApps sets cfg's apps from --apps, or else keeps the saved apps, or
// else takes the detected ones. As in interactive setup, an app it includes
// is no longer declined, and one --apps leaves out of the saved apps is.
func setupApps(cfg *config.Config, apps string, detected []string) error {
	var chosen []string
	switch {
	case apps != "":
		for _, app := range strings.Split(apps, ",") {
			app = strings.TrimSpace(app)
			if !containsString(allHarnesses, app) {
				return fmt.Errorf("--apps takes codex, claude, and cursor, not %q", app)
			}
			if !containsString(chosen, app) {
				chosen = append(chosen, app)
			}
		}
	case len(cfg.Harnesses) > 0:
		chosen = cfg.Harnesses
	default:
		for _, app := range detected {
			if !containsString(cfg.DeclinedHarnesses, app) {
				chosen = append(chosen, app)
			}
		}
		if len(chosen) == 0 {
			return errors.New("no apps were found on this Mac; pass --apps (codex, claude, cursor)")
		}
	}
	var ordered, declined []string
	for _, app := range allHarnesses {
		if containsString(chosen, app) {
			ordered = append(ordered, app)
		} else if containsString(cfg.DeclinedHarnesses, app) || containsString(cfg.Harnesses, app) {
			declined = append(declined, app)
		}
	}
	cfg.Harnesses, cfg.DeclinedHarnesses = ordered, declined
	return nil
}

// setupProjects includes each --project directory in cfg, beside the
// projects already saved.
func setupProjects(cfg *config.Config, paths []string, userHome string) error {
	for _, path := range paths {
		root, err := projectDir(path, userHome)
		if err != nil {
			return fmt.Errorf("--project %w", err)
		}
		included := false
		for i := range cfg.Archive.Projects {
			if cfg.Archive.Projects[i].Root == root {
				cfg.Archive.Projects[i].Included, included = true, true
			}
		}
		if !included {
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true})
		}
	}
	if includedProjects(cfg.Archive.Projects) == 0 {
		return errors.New("no project is included; pass --project DIR")
	}
	return nil
}

// setupStorageFromFlags sets cfg's storage from the flags, or keeps the
// saved storage when no storage flag was passed. For R2, the returned
// credentials carry the access key ID when a new key is to be stored; the
// secret is read later, once everything else checks out.
func setupStorageFromFlags(cfg *config.Config, opts setupOptions, env Env) (credentials.R2Credentials, error) {
	var secret credentials.R2Credentials
	if !opts.storageFlagsSupplied {
		if cfg.Storage.Provider == "" {
			return secret, errors.New("storage is not set up yet; pass --provider r2 or --provider s3 and the bucket's details")
		}
		return secret, nil
	}
	previous := cfg.Storage
	next := credentials.Config{Provider: opts.provider, Bucket: opts.bucket}
	if next.Provider == previous.Provider {
		next.Prefix = previous.Prefix
	}
	next.Prefix = firstNonEmpty(next.Prefix, defaultPrefix)
	//lint:ignore LV1001 --provider is raw user input; anything else is refused below
	switch opts.provider {
	case credentials.ProviderR2:
		if opts.awsProfile != "" || opts.region != "" {
			return secret, errors.New("--aws-profile and --region are for --provider s3")
		}
		if opts.r2Account == "" {
			return secret, errors.New("--provider r2 needs --r2-account (the account ID, or the bucket URL from the dashboard)")
		}
		loc, err := credentials.ParseR2Location(opts.r2Account)
		if err != nil {
			return secret, fmt.Errorf("--r2-account: %w", err)
		}
		if loc.Bucket != "" && next.Bucket != "" && loc.Bucket != next.Bucket {
			return secret, fmt.Errorf("--bucket %s differs from the bucket in --r2-account's URL (%s)", next.Bucket, loc.Bucket)
		}
		next.Bucket = firstNonEmpty(next.Bucket, loc.Bucket)
		if next.R2Endpoint, err = credentials.R2Endpoint(loc.Endpoint, loc.AccountID); err != nil {
			return secret, fmt.Errorf("--r2-account: %w", err)
		}
		next.R2AccountID = loc.AccountID
		secret.AccessKeyID = firstNonEmpty(opts.r2KeyID, lookupEnvTrimmed(env, envR2AccessKeyID))
		if secret.AccessKeyID == "" {
			if previous.Provider != credentials.ProviderR2 || previous.R2CredentialRef == "" {
				return secret, fmt.Errorf("--provider r2 needs the access key ID: pass --r2-access-key-id or set %s", envR2AccessKeyID)
			}
			next.R2CredentialRef = previous.R2CredentialRef
		}
	case credentials.ProviderS3:
		if opts.r2Account != "" || opts.r2KeyID != "" {
			return secret, errors.New("--r2-account and --r2-access-key-id are for --provider r2")
		}
		if opts.awsProfile == "" {
			return secret, errors.New("--provider s3 needs --aws-profile")
		}
		next.AWSProfile, next.Region = opts.awsProfile, opts.region
		if next.Region == "" {
			profiles, _ := env.awsProfiles()
			for _, profile := range profiles {
				if profile.Name == next.AWSProfile {
					next.Region = profile.Region
				}
			}
			if next.Region == "" {
				return secret, fmt.Errorf("AWS profile %s names no region; pass --region", next.AWSProfile)
			}
		}
	case "":
		return secret, errors.New("--bucket and the other storage flags need --provider r2 or --provider s3")
	default:
		return secret, fmt.Errorf("--provider must be r2 or s3, not %q", opts.provider)
	}
	if next.Bucket == "" {
		return secret, errors.New("--bucket is required")
	}
	cfg.Storage = next
	return secret, nil
}

// readR2Secret reads the R2 secret access key from its environment variable,
// or else from standard input: hidden when that is a terminal, otherwise its
// first line.
func readR2Secret(p *prompter, stdin io.Reader, env Env) (string, error) {
	if value := lookupEnvTrimmed(env, envR2SecretAccessKey); value != "" {
		return value, nil
	}
	label := ""
	if env.isTerminal(stdin) {
		label = "Secret access key (hidden): "
	}
	value, err := p.secret(label)
	if err != nil || value == "" {
		return "", fmt.Errorf("the R2 secret access key is needed: set %s or pass it on standard input", envR2SecretAccessKey)
	}
	return value, nil
}

func lookupEnvTrimmed(env Env, key string) string {
	value, _ := env.lookupEnv(key)
	return strings.TrimSpace(value)
}
