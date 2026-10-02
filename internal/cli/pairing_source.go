package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func pairingAgentRefusal(env Env) error {
	for _, marker := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CURSOR_AGENT"} {
		if _, present := env.lookupEnv(marker); present {
			return errors.New("pairing refuses to run inside a coding agent; run it in a separate terminal")
		}
	}
	return nil
}

func pairingInvocation(args []string) bool {
	if len(args) > 1 && args[0] == "machines" && args[1] == "add" {
		return true
	}
	if len(args) > 0 && args[0] == "setup" {
		for _, arg := range args[1:] {
			//lint:ignore LV1001 These are public flag spellings rather than domain states.
			if arg == "--pair" || arg == "-pair" || arg == "--pair-file" || arg == "-pair-file" || strings.HasPrefix(arg, "--pair=") || strings.HasPrefix(arg, "--pair-file=") || strings.HasPrefix(arg, "-pair=") || strings.HasPrefix(arg, "-pair-file=") {
				return true
			}
		}
	}
	return false
}

func runPairingAdd(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	fs := env.newCommandFlags("machines add", errOut)
	name := fs.String("name", "", "new machine name")
	share := fs.Bool("share-key", false, "explicitly share this R2 key (beta; cannot revoke the recipient independently)")
	expires := fs.Duration("expires", 15*time.Minute, "pairing expiry, 5m through 24h")
	printBundle := fs.Bool("print", false, "print the encrypted bundle instead of using the clipboard")
	file := fs.String("file", "", "write a new private bundle file, refusing overwrite")
	yes := fs.Bool("yes", false, "scripted delivery: print bundle and code separately without questions")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if *expires < 5*time.Minute || *expires > 24*time.Hour || (*yes && *name == "") || (*file != "" && *printBundle) {
		return fs.usageError("use expiry 5m..24h, --name with --yes, and one of --print or --file")
	}
	if err := pairingAgentRefusal(env); err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	if !*yes && (!env.interactive(stdin) || !env.interactive(out)) {
		terminal.Println(errOut, "machines add needs terminal input and output, or deliberate --yes scripted delivery")
		return 1
	}
	p := newPrompter(stdin, out)
	if *name == "" {
		value, err := p.ask("Name for the new machine", false, nil, -1, ": ")
		if err != nil {
			terminal.Println(errOut, "no name supplied")
			return 1
		}
		*name = value
	}
	if !pairing.ValidName(*name) {
		return fs.usageError("machine names use 1..40 lowercase letters, digits, or hyphens")
	}
	cfg, home, userHome, err := preparePairingSource(*name, *share, env)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	terminal.Println(out, "Storage check passed. Shared-key beta: no independent R2 revocation.")
	payload, err := sourcePairingPayload(cfg, *name, userHome, *expires, env)
	if err != nil {
		terminal.Println(errOut, "cannot prepare portable settings: "+err.Error())
		return 1
	}
	code, err := pairing.NewCode()
	if err != nil {
		terminal.Println(errOut, "cannot generate pairing code")
		return 1
	}
	bundle, err := pairing.Seal(payload, code)
	if err != nil {
		terminal.Println(errOut, err.Error())
		return 1
	}
	release, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		terminal.Println(errOut, "cannot lock pairing ledger")
		return 1
	}
	defer release()
	kind := pairingAWSProfile
	keyID, credentialRef := "", ""
	if cfg.Storage.Provider == credentials.ProviderR2 {
		kind = pairingSharedR2
		keyID = payload.AccessKeyID
		credentialRef = cfg.Storage.R2CredentialRef
	}
	ledger := pairingLedger{Version: 1, PairingID: payload.PairingID, RecipientID: payload.RecipientID, IssuerID: payload.IssuerID, Name: *name, DestinationID: config.DestinationID(cfg.Storage), Kind: kind, State: pairingPrepared, AccessKeyID: keyID, CredentialRef: credentialRef, CreatedAt: payload.CreatedAt, ExpiresAt: payload.ExpiresAt}
	if err = savePairingLedger(home, ledger); err != nil {
		terminal.Println(errOut, "cannot persist pairing preparation")
		return 1
	}
	ledger.State = pairingDeliveryIntent
	if err = savePairingLedger(home, ledger); err != nil {
		terminal.Println(errOut, "cannot persist delivery intent")
		return 1
	}
	// Any failure after durable intent is uncertain delivery, never cancellation.
	if *file != "" {
		err = writePairingFile(*file, bundle)
	} else if *printBundle || *yes {
		_, err = fmt.Fprintln(out, bundle)
	} else {
		err = env.pairClipboardWrite([]byte(bundle))
		if err == nil {
			defer env.clearPairClipboard(bundle)
			terminal.Println(out, "Encrypted bundle copied. Clipboard managers may retain it.")
		}
	}
	if err != nil {
		terminal.Println(errOut, "pairing delivery failed or is uncertain; its key remains valid and tracked")
		return 1
	}
	ledger.State = pairingDelivered
	ledger.DeliveredAt = env.now().UTC()
	if err = savePairingLedger(home, ledger); err != nil {
		terminal.Println(errOut, "delivery occurred; ledger update pending (delivery intent retained)")
		return 1
	}
	return finishPairingDelivery(p, code, ledger, home, *yes, out, errOut, env)
}

func finishPairingDelivery(p *prompter, code string, ledger pairingLedger, home string, yes bool, out, errOut io.Writer, env Env) int {
	if yes {
		words, err := pairing.CodeWords(code)
		if err != nil {
			terminal.Println(errOut, "code delivery uncertain; pairing remains tracked")
			return 1
		}
		if _, err := fmt.Fprintln(out, "Pairing code (deliver separately): "+strings.Join(words, " ")); err != nil {
			terminal.Println(errOut, "code delivery uncertain; pairing remains tracked")
			return 1
		}
		return 0
	}
	terminal.Printf(out, "Bundle expires at %s. On %s, run agent-archive setup --pair.\n", ledger.ExpiresAt.Format(time.RFC3339), ledger.Name)
	for {
		choice, err := p.menu("Pairing code", "show", option{"show", "Show code on a cleared alternate screen"}, option{"done", "Done"}, option{"cancel", "Cancel pairing (shared credentials stay active)"})
		if err != nil {
			terminal.Println(errOut, "pairing remains delivered; code discarded on exit")
			return 1
		}
		if choice == "done" {
			return 0
		}
		if choice == "cancel" {
			ledger.State = pairingCancelled
			if err = savePairingLedger(home, ledger); err != nil {
				terminal.Println(errOut, "cannot record cancellation")
				return 1
			}
			terminal.Println(out, "Pairing cancelled locally. Shared R2 access remains active; replace the shared key on every user to revoke it.")
			return 0
		}
		if err = showPairingCode(p, code, env); err != nil {
			terminal.Println(errOut, "pairing remains delivered; code discarded on exit")
			return 1
		}
	}
}

func sourcePairingPayload(cfg config.Config, name, userHome string, expiry time.Duration, env Env) (pairing.Payload, error) {
	if !pairing.ValidID(cfg.MachineID) {
		return pairing.Payload{}, errors.New("source machine identity is invalid; run setup first")
	}
	id, err := local.ID()
	if err != nil {
		return pairing.Payload{}, err
	}
	recipient, err := local.ID()
	if err != nil {
		return pairing.Payload{}, err
	}
	now := env.now().UTC().Truncate(time.Second)
	storage, key, err := pairingSourceStorage(cfg, env)
	if err != nil {
		return pairing.Payload{}, err
	}
	issuerName := cfg.MachineName
	if issuerName == "" {
		issuerName = "unnamed-" + cfg.MachineID[:4]
	}
	inclusions, exclusions, err := exportPairingScope(context.Background(), cfg, userHome, env)
	if err != nil {
		return pairing.Payload{}, err
	}
	p := pairing.Payload{Version: 1, PairingID: id, RecipientID: recipient, IssuerID: cfg.MachineID, IssuerName: issuerName, Name: name, CreatedAt: now, ExpiresAt: now.Add(expiry), Apps: cfg.Harnesses, RetentionDays: cfg.RetentionDays, RequireSkillUse: cfg.RequireSkillUse, SkillEvidence: string(cfg.EffectiveSkillEvidence()), NoSkills: cfg.NoSkills, HandoffArgs: cfg.Handoff.Args, HandoffDefault: cfg.Handoff.DefaultTo, Storage: storage, AccessKeyID: key.AccessKeyID, SecretAccessKey: key.SecretAccessKey, Inclusions: inclusions, Exclusions: exclusions}
	return p, p.Validate()
}

func pairingNameAvailable(store storage.ObjectStore, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), machines.Timeout)
	defer cancel()
	result := machines.List(ctx, store)
	if result.Partial || len(result.Unreadable) > 0 {
		return errors.New("machine-name discovery is incomplete; resolve omitted records before pairing")
	}
	for _, record := range result.Records {
		if record.Name == name {
			return errors.New("machine name is already observed; choose another name")
		}
	}
	return nil
}

func writePairingFile(path, bundle string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = io.WriteString(f, bundle+"\n"); err != nil {
		return err
	}
	return f.Sync()
}

func showPairingCode(p *prompter, code string, env Env) error {
	words, err := pairing.CodeWords(code)
	if err != nil {
		return err
	}
	if !env.interactive(p.out) {
		return errors.New("pairing code display needs terminal output")
	}
	// Use checked writes for secret-bearing output. Always restore the screen.
	if _, err := io.WriteString(p.out, "\x1b[?1049h\x1b[2J\x1b[H"); err != nil {
		return err
	}
	signals, stop := env.interrupts()
	screen := &altScreen{out: p.out, active: true, stop: stop, done: make(chan struct{}), exit: env.exit}
	screen.restoreOnLeave(func() { _, _ = io.WriteString(p.out, "\x1b[2J\x1b[H") })
	defer screen.leave()
	go func() {
		select {
		case sig := <-signals:
			if sig != nil {
				screen.exitForSignal(sig)
			}
		case <-screen.done:
		}
	}()
	screen.mu.Lock()
	if !screen.active {
		screen.mu.Unlock()
		return errors.New("pairing code display interrupted")
	}
	_, err = fmt.Fprintln(p.out, "Pairing code: "+strings.Join(words, " ")+"\nType the first three characters of each word (yo- for yo-yo). Recording or screen sharing may capture it.\nPress Enter to hide.")
	screen.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = boundedPairingLine(p.in, 512)
	return err
}

func (e Env) pairClipboardWrite(data []byte) error {
	if err := e.clipboard(data); err != nil {
		return errors.New("clipboard unavailable; use --print or --file")
	}
	return nil
}

func (e Env) clearPairClipboard(bundle string) {
	var data []byte
	var err error
	if e.PairingClipboardRead != nil {
		data, err = e.PairingClipboardRead()
	} else if e.Clipboard == nil {
		data, err = e.pairClipboardRead()
	} else {
		return
	}
	if err == nil && string(data) == bundle {
		_ = e.pairClipboardWrite(nil)
	}
}

func (e Env) pairClipboardRead() ([]byte, error) {
	provider, _, err := e.clipboardCommand()
	if err != nil {
		return nil, err
	}
	name := filepath.Base(provider)
	var args []string
	//lint:ignore LV1001 These are names of external clipboard executables selected by clipboardCommand.
	switch name {
	case "pbcopy":
		name = "pbpaste"
	case "wl-copy":
		name = "wl-paste"
		args = []string{"--no-newline", "--type", "text/plain"}
	case "xclip":
		args = []string{"-selection", "clipboard", "-o"}
	case "xsel":
		args = []string{"--clipboard", "--output"}
	default:
		return nil, errors.New("clipboard provider cannot be checked")
	}
	program, err := e.lookPath(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = e.environ()
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var output pairingClipboardOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || output.over {
		return nil, errors.New("clipboard could not be checked")
	}
	return output.buffer.Bytes(), nil
}

type pairingClipboardOutput struct {
	buffer bytes.Buffer
	over   bool
}

func (o *pairingClipboardOutput) Write(p []byte) (int, error) {
	room := pairing.MaxBundle - o.buffer.Len()
	if len(p) > room {
		o.over = true
	}
	if room > 0 {
		o.buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func pairingInvocationError(args []string, env Env) error {
	if pairingInvocation(args) {
		return pairingAgentRefusal(env)
	}
	return nil
}

func runMachinesWithInput(args []string, stdin io.Reader, out, errOut io.Writer, env Env) int {
	if len(args) > 0 && args[0] == "add" {
		return runPairingAdd(args[1:], stdin, out, errOut, env)
	}
	return runMachinesCommand(args, stdin, out, errOut, env)
}

func preparePairingSource(name string, share bool, env Env) (config.Config, string, string, error) {
	bad := func(message string) (config.Config, string, string, error) {
		return config.Config{}, "", "", errors.New(message)
	}
	home, err := env.readHome()
	if err != nil {
		return bad("cannot locate archive configuration")
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled {
		return bad("run setup on this machine before creating a pairing")
	}
	if cfg.Storage.Provider == credentials.ProviderR2 && !share {
		return bad("Shared-key R2 beta requires --share-key; this recipient cannot be revoked independently.")
	}
	if cfg.Storage.Provider == credentials.ProviderS3 && share {
		return bad("--share-key is for R2; S3 transfers settings and a profile name only")
	}
	if cfg.MachineName == name {
		return bad("choose a name different from the source machine")
	}
	prior, err := readPairingLedgers(home)
	if err != nil {
		return bad(err.Error())
	}
	for _, claim := range prior {
		if claim.Name == name && claim.State != pairingCancelled && claim.State != pairingExpired && !env.now().After(claim.ExpiresAt) {
			return bad("that name already has an outstanding pairing; choose another name")
		}
	}
	if connect, access := verifyStorage(&cfg, env); connect != nil || access != nil {
		return bad("source storage check failed; no pairing was created")
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return bad("cannot inspect machine names")
	}
	if err = pairingNameAvailable(store, name); err != nil {
		return bad(err.Error())
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return bad("cannot resolve project hints")
	}
	return cfg, home, userHome, nil
}

func pairingSourceStorage(cfg config.Config, env Env) (pairing.Storage, credentials.R2Credentials, error) {
	storage := pairing.Storage{Provider: cfg.Storage.Provider, Bucket: cfg.Storage.Bucket, Prefix: cfg.Storage.Prefix, Region: cfg.Storage.Region, R2Account: cfg.Storage.R2AccountID, R2Endpoint: cfg.Storage.R2Endpoint, AWSProfile: cfg.Storage.AWSProfile}
	if cfg.Storage.Provider != credentials.ProviderR2 {
		return storage, credentials.R2Credentials{}, nil
	}
	kc, err := env.credentialStore()
	if err != nil {
		return storage, credentials.R2Credentials{}, errors.New("cannot read source R2 credential")
	}
	key, err := credentials.LoadStored(context.Background(), kc, cfg.Storage.R2CredentialRef)
	if err != nil {
		return storage, key, errors.New("cannot read source R2 credential")
	}
	return storage, key, nil
}
