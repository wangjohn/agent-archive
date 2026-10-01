package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func boundedPairingLine(r *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > limit {
			return "", fmt.Errorf("pairing input exceeds its size limit")
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("pairing input could not be read")
		}
		if len(line) == 0 {
			return "", io.EOF
		}
		return strings.TrimSpace(string(line)), nil
	}
}

func readPairingBundle(p *prompter, opts setupOptions, stdin io.Reader) (string, error) {
	if opts.pairFile != "" {
		reader := stdin
		var f *os.File
		if opts.pairFile != "-" {
			var err error
			f, err = os.Open(opts.pairFile)
			if err != nil {
				return "", fmt.Errorf("cannot open pairing file")
			}
			defer func() { _ = f.Close() }()
			reader = f
		}
		data, err := io.ReadAll(io.LimitReader(reader, pairing.MaxBundle+2))
		if err != nil || len(data) > pairing.MaxBundle+1 {
			return "", fmt.Errorf("pairing file is oversized or unreadable")
		}
		return strings.TrimSpace(string(data)), nil
	}
	if opts.yes {
		return "", fmt.Errorf("--yes pairing needs --pair-file PATH or --pair-file -")
	}
	terminal.Print(p.out, "Paste the encrypted pairing bundle: ")
	return boundedPairingLine(p.in, pairing.MaxBundle+1)
}

func setupPairing(opts setupOptions, stdin io.Reader, out, errOut io.Writer, env Env) error {
	if err := pairingAgentRefusal(env); err != nil {
		return err
	}
	p := newPrompter(stdin, out)
	p.now = env.now
	bundle, err := readPairingBundle(p, opts, stdin)
	if err != nil {
		return err
	}
	if _, err = pairing.Inspect(bundle); err != nil {
		return err
	}
	code, err := readPairingCode(opts, p, stdin, env)
	if err != nil {
		return err
	}
	payload, err := pairing.Open(bundle, code, env.now())
	if err != nil {
		return err
	}
	// Validation above is direct: setup's transaction does not call Config.Save.
	home, err := env.home()
	if err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	if err = recoverSetup(home, env); err != nil {
		return err
	}
	existing, found, err := config.Load(home)
	if err != nil {
		return err
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	if problem := env.temporaryExecutableProblem(exe); problem != "" {
		return fmt.Errorf("%s; install a lasting executable before pairing", problem)
	}
	if result, refused := refuseNetworkHome(opts, out, errOut, env); refused {
		return fmt.Errorf("network home refused (exit %d)", result)
	}
	cfg, err := reviewPairingDestination(p, payload, existing, found, opts, env)
	if err != nil {
		return err
	}
	cfg, err = pairingCaptureSettings(p, payload, cfg, existing, userHome, opts, env)
	if err != nil {
		return err
	}
	cfg, err = reviewPairingSettings(p, payload, cfg, existing, found, userHome, opts, env)
	if err != nil {
		return err
	}
	pairedAt := env.now().UTC()
	kind := "aws_profile"
	sharedWith := ""
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kind = "r2_shared"
		sharedWith = payload.IssuerID
	}
	cfg.MachineName = payload.Name
	cfg.MachineAssignment = &config.MachineAssignment{DestinationID: cfg.DestinationID(), Kind: kind, AccessKeyID: payload.AccessKeyID, RecipientID: payload.RecipientID, IssuerID: payload.IssuerID, SharedWith: sharedWith, PairingID: payload.PairingID, PairedFrom: payload.IssuerID, PairedAt: &pairedAt}
	if err = cfg.ValidateMachine(); err != nil {
		return fmt.Errorf("invalid paired machine assignment")
	}

	cfg, err = stagePairingCredential(home, cfg, payload, env)
	if err != nil {
		return err
	}
	checks := preflight(env, home, userHome, preflightScope{apps: cfg.Harnesses, kept: existing.Harnesses, r2: cfg.Storage.Provider == credentials.ProviderR2, credentialRef: cfg.Storage.R2CredentialRef})
	checks.print(p)
	if checks.blocked() {
		return &preflightError{checks: checks, yes: opts.yes}
	}
	if err = runStorageCheck(p, &cfg, env); err != nil {
		return fmt.Errorf("paired storage check failed; existing capture is unchanged, retry this pairing or finish/discard the saved setup")
	}
	if err = reviewWithoutQuestions(home, existing, cfg, p, env); err != nil {
		return err
	}
	cfg.ImportedHarnesses = carriedImportedHarnesses(existing.ImportedHarnesses, cfg.Harnesses, nil)
	skills := planSkillOptOut(env, home, userHome, exe, existing, cfg)
	discoveries := env.discoverApplications(userHome)
	if err = applySetup(home, userHome, exe, existing, &cfg, nil, env); err != nil {
		return fmt.Errorf("pairing setup did not commit: %w; retry the same pairing or finish/discard the saved setup", err)
	}
	if err = finishSetup(p, errOut, home, cfg, existing.Paused, discoveries, env.now(), setupFinish{env: env, userHome: userHome, offerImport: !opts.yes, skills: skills}); err != nil {
		return err
	}
	terminal.Printf(out, "Paired with %s. This machine is %s. Shared R2 access cannot be revoked independently.\n", payload.IssuerName, payload.Name)
	return nil
}

func readPairingCode(opts setupOptions, p *prompter, stdin io.Reader, env Env) (string, error) {
	var err error
	var code string
	if opts.yes {
		var present bool
		code, present = env.lookupEnv("AGENT_ARCHIVE_PAIRING_CODE")
		unsetErr := env.unsetEnv("AGENT_ARCHIVE_PAIRING_CODE")
		if unsetErr != nil {
			return "", fmt.Errorf("cannot remove AGENT_ARCHIVE_PAIRING_CODE from this process")
		}
		if !present {
			return "", fmt.Errorf("--yes needs AGENT_ARCHIVE_PAIRING_CODE; deliver it separately from the bundle")
		}
	} else {
		if env.PairingCode != nil {
			code, err = env.PairingCode()
		} else if env.interactive(stdin) {
			code, err = p.secret("Pairing code (hidden): ")
		} else {
			tty, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if e != nil {
				return "", fmt.Errorf("pairing code needs a terminal, or --yes with AGENT_ARCHIVE_PAIRING_CODE")
			}
			defer func() { _ = tty.Close() }()
			code, err = newPrompter(tty, tty).secret("Pairing code (hidden): ")
		}
		if err != nil {
			return "", fmt.Errorf("pairing code could not be read")
		}
	}
	return code, nil
}

func reviewPairingDestination(p *prompter, payload pairing.Payload, existing config.Config, found bool, opts setupOptions, env Env) (config.Config, error) {
	var err error
	cfg := existing
	cfg.Storage = credentials.Config{Provider: payload.Storage.Provider, Bucket: payload.Storage.Bucket, Prefix: payload.Storage.Prefix, Region: payload.Storage.Region, AWSProfile: payload.Storage.AWSProfile, R2AccountID: payload.Storage.R2Account, R2Endpoint: payload.Storage.R2Endpoint}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		cfg.Storage.R2Endpoint, err = credentials.R2Endpoint(cfg.Storage.R2Endpoint, cfg.Storage.R2AccountID)
		if err != nil {
			return cfg, fmt.Errorf("paired R2 endpoint is invalid")
		}
	}
	terminal.Printf(p.out, "Shared-key beta from %s. Proposed machine: %s.\nSessions will upload to %s bucket %s, folder %s, account %s, endpoint %s.\n", payload.IssuerName, payload.Name, cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix, cfg.Storage.R2AccountID, cfg.Storage.R2Endpoint)
	if found {
		terminal.Printf(p.out, "Prior destination: %s bucket %s, folder %s.\n", existing.Storage.Provider, existing.Storage.Bucket, existing.Storage.Prefix)
	}
	changed := found && existing.Storage.Provider != "" && !destinationEqual(existing.Storage, cfg.Storage)
	if changed && opts.yes {
		return cfg, fmt.Errorf("paired destination differs from this machine's destination; run interactive pairing to approve the exact change")
	}
	if changed {
		consent, e := p.yesNo("Explicitly replace the prior destination with the displayed destination?", false)
		if e != nil {
			return cfg, e
		}
		if !consent {
			return cfg, fmt.Errorf("destination change declined; nothing was changed")
		}
	}
	if cfg.Storage.Provider == credentials.ProviderS3 {
		if !validRegion(cfg.Storage.Region) {
			return cfg, fmt.Errorf("paired AWS region is invalid; configure the source and create a new pairing")
		}
		profiles, e := env.awsProfiles()
		if e != nil {
			return cfg, fmt.Errorf("cannot read local AWS profiles")
		}
		present := false
		for _, profile := range profiles {
			if profile.Name == cfg.Storage.AWSProfile {
				present = true
			}
		}
		if !present {
			return cfg, fmt.Errorf("AWS profile %s is missing; configure it with aws configure --profile %s or aws configure sso --profile %s, then retry", cfg.Storage.AWSProfile, shellWord(cfg.Storage.AWSProfile), shellWord(cfg.Storage.AWSProfile))
		}
	}
	return cfg, nil
}

func pairingCaptureSettings(p *prompter, payload pairing.Payload, cfg, existing config.Config, userHome string, opts setupOptions, env Env) (config.Config, error) {
	var err error
	cfg.RetentionDays = payload.RetentionDays
	cfg.RequireSkillUse = payload.RequireSkillUse
	cfg.SkillEvidence = config.SkillEvidence(payload.SkillEvidence)
	cfg.NoSkills = payload.NoSkills
	cfg.Handoff = config.HandoffConfig{Args: payload.HandoffArgs, DefaultTo: payload.HandoffDefault}
	for _, agent := range []string{"claude", "codex", "cursor"} {
		if args := cfg.Handoff.Args[agent]; len(args) > 0 {
			quoted := make([]string, len(args))
			for i, arg := range args {
				quoted[i] = shellWord(arg)
			}
			terminal.Printf(p.out, "Handoff %s arguments: %s\n", agent, strings.Join(quoted, " "))
		}
	}
	cfg.Archive.Projects, err = pairScope(p, payload, existing, userHome, env, opts.yes)
	if err != nil {
		return cfg, err
	}
	if problems := setupProjects(&cfg, opts.projects, userHome); len(problems) > 0 {
		return cfg, answersError(problems)
	}
	detected := env.detectHarnesses(userHome)
	var apps []string
	for _, app := range payload.Apps {
		if slices.Contains(detected, app) || slices.Contains(existing.Harnesses, app) {
			apps = append(apps, app)
		} else {
			terminal.Printf(p.out, "Source app %s is not found here (skipped); install it and rerun ordinary setup to add it.\n", app)
		}
	}
	for _, app := range existing.Harnesses {
		if !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	if len(apps) == 0 {
		return cfg, fmt.Errorf("none of the source apps is installed; install an app and retry")
	}
	cfg.Harnesses = apps
	return cfg, nil
}

func reviewPairingSettings(p *prompter, payload pairing.Payload, cfg, existing config.Config, found bool, userHome string, opts setupOptions, env Env) (config.Config, error) {
	var err error
	approvedStorage := cfg.Storage
	if !opts.yes {
		for {
			showSetupReview(p, cfg, setupReview{existing: existing, reconfiguring: found, hookFiles: env.hookFiles(userHome), installedHookFiles: env.installedHookFiles(userHome, existing), userHome: userHome})
			choice, e := p.menu("Review pairing settings", "cancel", option{"save", "Save the displayed destination and capture settings"}, option{"edit", "Edit settings"}, option{"cancel", "Cancel"})
			if e != nil {
				return cfg, e
			}
			if choice == "cancel" {
				return cfg, fmt.Errorf("pairing cancelled; nothing was changed")
			}
			if choice == "save" {
				break
			}
			draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2}
			if err = editSetupReview(p, &draft, userHome, backfilledProjects(env), func(config.Config) []backfill.KnownProject { return nil }); err != nil {
				return cfg, err
			}
			cfg = draft.Config
			if !destinationEqual(cfg.Storage, approvedStorage) {
				terminal.Printf(p.out, "Edited destination: %s bucket %s, folder %s, account %s, endpoint %s.\n", cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix, cfg.Storage.R2AccountID, cfg.Storage.R2Endpoint)
				consent, e := p.yesNo("Explicitly approve this edited destination?", false)
				if e != nil {
					return cfg, e
				}
				if !consent {
					return cfg, fmt.Errorf("destination change declined; nothing was changed")
				}
				if cfg.Storage.Provider != payload.Storage.Provider || cfg.Storage.R2AccountID != payload.Storage.R2Account {
					return cfg, fmt.Errorf("credential source edited; configure it with ordinary setup before pairing")
				}
				approvedStorage = cfg.Storage
			}
		}
	}
	return cfg, nil
}

func stagePairingCredential(home string, cfg config.Config, payload pairing.Payload, env Env) (config.Config, error) {
	priorDraft, have, problem, err := readDraft(home)
	if err != nil {
		return cfg, err
	}
	if problem != "" {
		return cfg, fmt.Errorf("unfinished setup cannot be read; finish or discard it before pairing")
	}
	ref := "pairing-" + payload.PairingID
	if have && (priorDraft.PairingID != payload.PairingID || !destinationEqual(priorDraft.Config.Storage, cfg.Storage)) {
		return cfg, fmt.Errorf("another unfinished setup exists; finish or discard it before pairing")
	}
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kc, e := env.credentialStore()
		if e != nil {
			return cfg, openCredentialStoreError(credentialOS, e)
		}
		cfg.Storage.R2CredentialRef = ref
		draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2, PairingID: payload.PairingID, CredentialRef: ref, StagedRefs: []string{ref}}
		if err = local.Write(draftPath(home), draft); err != nil {
			return cfg, err
		}
		value := credentials.R2Credentials{AccessKeyID: payload.AccessKeyID, SecretAccessKey: payload.SecretAccessKey}
		prior, e := credentials.LoadStored(context.Background(), kc, ref)
		if e != nil && !errors.Is(e, credentials.ErrMissingCredential) {
			return cfg, fmt.Errorf("cannot read staged pairing credential; retry or discard unfinished setup")
		}
		if e == nil && prior != value {
			return cfg, fmt.Errorf("staged pairing identity mismatched; discard unfinished setup before retrying")
		}
		if errors.Is(e, credentials.ErrMissingCredential) {
			if err = kc.Save(context.Background(), ref, value); err != nil {
				return cfg, fmt.Errorf("cannot stage paired credential; retry the same pairing")
			}
		}
	}
	payload.SecretAccessKey = ""
	return cfg, nil
}
