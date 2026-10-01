package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/machines"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const machineRegistrationFile = "machine-registration.json"

type machineRegistration struct {
	DestinationID string    `json:"destination_id"`
	Fingerprint   string    `json:"fingerprint"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
	Pending       bool      `json:"pending"`
}

func machineRecord(ctx context.Context, cfg config.Config, env Env) (machines.Record, error) {
	keyID := ""
	if cfg.Storage.Provider == credentials.ProviderR2 && (cfg.MachineAssignment == nil || cfg.MachineAssignment.DestinationID != cfg.DestinationID()) {
		kc, err := env.credentialStore()
		if err != nil {
			return machines.Record{}, err
		}
		key, err := kc.Load(ctx, cfg.Storage.R2CredentialRef)
		if err != nil {
			return machines.Record{}, err
		}
		keyID = key.AccessKeyID
	}
	return machines.Build(cfg, string(env.operatingSystem())+"/"+runtime.GOARCH, versionString(), keyID, env.now())
}

// publishMachineLocked runs with collector.lock held and never changes config.
// Failures remain independent of capture and retention. Acknowledgements bind
// destination and stable record content, so a change or failed PUT retries.
func publishMachineLocked(ctx context.Context, home string, cfg config.Config, env Env, store storage.ObjectStore, force bool) error {
	if !cfg.Archive.Enabled || (cfg.Paused && !force) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, machines.Timeout)
	defer cancel()
	var ack machineRegistration
	_ = local.Read(filepath.Join(home, machineRegistrationFile), &ack)
	record, err := machineRecord(ctx, cfg, env)
	fingerprint := ""
	if err == nil {
		fingerprint = machines.Fingerprint(record)
	}
	if err == nil && !force && !ack.Pending && ack.DestinationID == cfg.DestinationID() && ack.Fingerprint == fingerprint && !ack.LastSuccess.IsZero() && env.now().Sub(ack.LastSuccess) >= 0 && env.now().Sub(ack.LastSuccess) < 24*time.Hour {
		return nil
	}
	next := machineRegistration{DestinationID: cfg.DestinationID(), Fingerprint: fingerprint, Pending: true}
	if ack.DestinationID == next.DestinationID {
		next.LastSuccess = ack.LastSuccess
	}
	if err == nil {
		if store == nil {
			store, err = env.openStoreContext(ctx, cfg)
		}
		if err == nil {
			err = machines.Publish(ctx, store, record)
		}
	}
	if err == nil {
		next.Pending = false
		next.LastSuccess = env.now().UTC()
	}
	if writeErr := local.Write(filepath.Join(home, machineRegistrationFile), next); writeErr != nil {
		return writeErr
	}
	return err
}

func publishMachineAfterSetup(home string, env Env) error {
	unlock, err := lockCollector(home, "machine registration", env.now())
	if err != nil {
		return err
	}
	defer unlock()
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found {
		return errNotSetUp
	}
	return publishMachineLocked(context.Background(), home, cfg, env, nil, true)
}

func registrationPending(home string, cfg config.Config) bool {
	if !cfg.Archive.Enabled || cfg.Paused {
		return false
	}
	var ack machineRegistration
	err := local.Read(filepath.Join(home, machineRegistrationFile), &ack)
	return err != nil || ack.Pending || ack.DestinationID != cfg.DestinationID()
}

func runMachinesCommand(args []string, out, errOut io.Writer, env Env) int {
	if len(args) > 0 && args[0] == "rename" {
		return runMachinesRename(args[1:], out, errOut, env)
	}
	fs := env.newCommandFlags("machines", errOut)
	asJSON := fs.Bool("json", false, "write informational machine records as JSON")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if !found {
		return machineCommandError(errOut, errNotSetUp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), machines.Timeout)
	defer cancel()
	store, err := env.openStoreContext(ctx, cfg)
	if err != nil {
		return machineCommandError(errOut, errors.New("could not open archive storage"))
	}
	result := machines.List(ctx, store)
	observePairingClaims(home, cfg.DestinationID(), result, env.now())
	if *asJSON {
		if err := json.NewEncoder(out).Encode(struct {
			machines.ListResult
			PairingWarnings []string `json:"pairing_warnings,omitempty"`
		}{result, pairingWarnings(home, env.now())}); err != nil {
			return machineCommandError(errOut, err)
		}
	} else {
		for _, r := range result.Records {
			own := ""
			if r.MachineID == cfg.MachineID {
				own = " (this machine)"
			}
			terminal.Printf(out, "%s  %s  %s  %s  Paired %s  Heartbeat %s%s\n", r.Name, r.MachineID, r.Platform, machineCredentialClaim(r, result.Records), machinePairingDate(r), r.HeartbeatAt.Format("2006-01-02"), own)
		}
		for _, u := range result.Unreadable {
			terminal.Printf(out, "Omitted %s: %s.\n", u.Key, u.Reason)
		}
		terminal.Println(out, "Not checked against the provider. Bucket records are untrusted claims, not proof of ownership or access removal.")
		terminal.Println(out, "Heartbeat is updated at most daily; it does not indicate current activity.")
		for _, warning := range pairingWarnings(home, env.now()) {
			terminal.Println(out, warning)
		}
	}
	if result.Partial || len(result.Unreadable) > 0 {
		return 1
	}
	return 0
}

func machineCommandError(out io.Writer, err error) int {
	terminal.Printf(out, "agent-archive: machines: %v\n", err)
	return 1
}

func runMachinesRename(args []string, out, errOut io.Writer, env Env) int {
	fs := env.newCommandFlags("machines rename", errOut)
	if !fs.parse(args) {
		return 2
	}
	words := fs.Args()
	if len(words) < 1 || len(words) > 2 {
		return fs.usageError("give a new name, optionally preceded by this machine's full ID or current name")
	}
	name := words[len(words)-1]
	if !config.ValidMachineName(name) {
		return fs.usageError("name must contain 1 to 40 lowercase letters, digits, or hyphens, starting with a letter or digit")
	}
	home, err := env.readHome()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	unlock, err := lockCollector(home, "machine rename", env.now())
	if err != nil {
		return machineCommandError(errOut, err)
	}
	defer unlock()
	if setupjournal.TransactionPending(home) {
		return machineCommandError(errOut, errors.New("setup recovery is pending; finish setup before renaming"))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if !found {
		return machineCommandError(errOut, errNotSetUp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), machines.Timeout)
	defer cancel()
	store, err := env.openStoreContext(ctx, cfg)
	if err != nil {
		return machineCommandError(errOut, errors.New("could not open archive storage"))
	}
	result := machines.List(ctx, store)
	if result.Partial || len(result.Unreadable) > 0 {
		return machineCommandError(errOut, errors.New("machine listing is incomplete; retry before choosing a name"))
	}
	if len(words) == 2 {
		r, e := machines.Select(result.Records, words[0])
		if e != nil {
			return machineCommandError(errOut, e)
		}
		if r.MachineID != cfg.MachineID {
			return machineCommandError(errOut, errors.New("rename must run on the machine being renamed"))
		}
	}
	for _, r := range result.Records {
		if r.Name == name && r.MachineID != cfg.MachineID {
			return machineCommandError(errOut, errors.New("name is already used; choose another name"))
		}
	}
	if !cfg.Archive.Enabled {
		return machineCommandError(errOut, errors.New("integrations are not installed; run setup before renaming"))
	}
	release, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return machineCommandError(errOut, err)
	}
	cfg.MachineName = name
	err = config.Save(home, cfg)
	release()
	if err != nil {
		return machineCommandError(errOut, err)
	}
	if err = publishMachineLocked(ctx, home, cfg, env, store, true); err != nil {
		terminal.Println(out, "Machine name saved. Machine registration pending; the collector will retry.")
		return 0
	}
	terminal.Printf(out, "This machine is now %s (%s).\n", name, cfg.MachineID)
	return 0
}

// chooseSetupMachineName changes only a first-setup draft after bounded duplicate
// observation. A blank answer keeps a neutral default without reading a hostname.
func chooseSetupMachineName(p *prompter, cfg *config.Config, env Env) error {
	for {
		name, err := p.line("Machine name (1-40 lowercase letters, digits or hyphens; blank keeps unnamed): ")
		if err != nil {
			return err
		}
		if name == "" {
			cfg.MachineName = ""
			return nil
		}
		if !config.ValidMachineName(name) {
			p.warn("Use 1 to 40 lowercase letters, digits, or hyphens, starting with a letter or digit.")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), machines.Timeout)
		store, err := env.openStoreContext(ctx, *cfg)
		if err != nil {
			cancel()
			return errors.New("could not check machine names; retry setup or keep the unnamed default")
		}
		result := machines.List(ctx, store)
		cancel()
		if result.Partial {
			return errors.New("machine listing is incomplete; retry setup or keep the unnamed default")
		}
		taken := false
		for _, r := range result.Records {
			if r.Name == name {
				taken = true
				break
			}
		}
		if taken {
			p.warn("Name is already used; choose another name.")
			continue
		}
		cfg.MachineName = name
		return nil
	}
}

func machinePairingDate(r machines.Record) string {
	if r.PairedAt == nil {
		return "unknown"
	}
	return r.PairedAt.Format("2006-01-02")
}

func machineCredentialClaim(r machines.Record, records []machines.Record) string {
	switch r.Credential.Kind {
	case config.MachineAssignmentAWSProfile:
		return "AWS profile (claim)"
	case config.MachineAssignmentR2Unknown:
		return "R2 ownership unknown"
	case config.MachineAssignmentR2Own:
		return "own R2 key (claim)"
	case config.MachineAssignmentR2Shared:
		identity := r.Credential.SharedWith
		for _, other := range records {
			if other.MachineID == identity {
				identity = other.Name + " (" + identity + ")"
				break
			}
		}
		return "shared R2 key with " + identity + " (claim; cannot revoke independently)"
	}
	return "unknown"
}
