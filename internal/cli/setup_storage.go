package cli

import (
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// storageNavigation carries an explicit destination, rather than a failure.
type storageNavigation string

func (n storageNavigation) Error() string { return string(n) }

const errUseExistingStorage storageNavigation = "use existing storage"

func storageHelp(p *prompter) {
	terminal.Println(p.out, "Step by step, for Cloudflare R2 and Amazon S3: "+bucketDocURL)
}

func promptStorage(p *prompter, existing credentials.Config, env Env, failedRegion string, keepInstalled ...bool) (credentials.Config, credentials.R2Credentials, bool, error) {
	offerKeep := len(keepInstalled) == 0 || keepInstalled[0]
	var secret credentials.R2Credentials
	if offerKeep && existing.Bucket != "" {
		for {
			terminal.Println(p.out, "Current storage: "+existing.Provider+" / "+existing.Bucket)
			choice, err := p.actions("Keep your current storage?", "keep", nil, []actionOption{{"keep", "", "Keep current storage"}, {"change", "c", "Change storage"}})
			if err != nil {
				return existing, secret, false, err
			}
			if choice == "keep" {
				if existing.Provider == credentials.ProviderR2 && !storedCredentialReadable(env, existing.R2CredentialRef) {
					return promptExistingR2(p, existing, env)
				}
				return existing, secret, false, nil
			}
			break
		}
	}
	def := existing.Provider
	if def == "" {
		def = defaultStorageProvider(env)
	}
	for {
		aliases := []option{{"r2-existing", ""}, {"s3-existing", ""}, {storageChoiceS3New, ""}}
		if experimentalR2Create(env) {
			aliases = append(aliases, option{guidedR2Choice, ""})
		}
		secondary := []actionOption{{"help", "h", "Setup instructions"}}
		if existing.Bucket != "" {
			terminal.Println(p.out, "Continue connecting the saved bucket, or choose a replacement.")
			if existing.Provider == credentials.ProviderS3 {
				secondary = append(secondary, actionOption{storageChoiceS3New, "n", "Create a replacement S3 bucket"})
			} else if experimentalR2Create(env) {
				secondary = append(secondary, actionOption{guidedR2Choice, "n", "Create a replacement R2 bucket"})
			}
		}
		choice, err := p.actions("Where should your archive live?", def, storageMenuOptions(), secondary, aliases...)
		if err != nil {
			return existing, secret, false, err
		}
		if choice == "help" {
			storageHelp(p)
			continue
		}
		provider := strings.Split(choice, "-")[0]
		mode := "new"
		switch choice {
		case "r2-existing", "s3-existing":
			mode = "existing"
		case guidedR2Choice, storageChoiceS3New:
		default:
			if provider == existing.Provider && existing.Bucket != "" {
				mode = "existing"
				break
			}
			mode, err = storageIntroduction(p, provider, env)
			if err != nil {
				return existing, secret, false, err
			}
			if mode == "back" {
				continue
			}
		}
		cfg := existing
		if cfg.Provider != provider || mode == "new" {
			cfg = credentials.Config{Provider: provider}
		}
		if mode == "new" && provider == credentials.ProviderR2 {
			var saved bool
			cfg, secret, saved, err = createR2Bucket(p, env)
			if err == nil {
				return cfg, secret, saved, nil
			}
			if errors.Is(err, errChooseStorageAgain) {
				continue
			}
			if !errors.Is(err, errUseExistingStorage) {
				return cfg, secret, false, err
			}
			cfg.Provider = provider
			mode = "existing"
		}
		if provider == credentials.ProviderR2 {
			return promptExistingR2(p, cfg, env)
		}
		err = promptS3Bucket(p, &cfg, env, failedRegion, mode == "new")
		if errors.Is(err, errChooseStorageAgain) {
			continue
		}
		if err != nil {
			return cfg, secret, false, err
		}
		cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)
		return cfg, secret, false, nil
	}
}

func storageIntroduction(p *prompter, provider string, env Env) (string, error) {
	for {
		title := "Set up Amazon S3"
		explanation := "Setup will create a bucket with Block Public Access using an AWS profile."
		def := "new"
		options := []actionOption{{"new", "", "Continue"}, {"existing", "e", "Use an existing bucket"}, {"back", "b", "Back"}, {"help", "h", "Setup instructions"}}
		if provider == credentials.ProviderR2 {
			title = "Set up Cloudflare R2"
			explanation = "Setup will create a private bucket and archive key. You'll provide a setup token once; it won't be saved."
			if !experimentalR2Create(env) {
				explanation = "Automatic R2 bucket creation is experimental and isn't enabled. Connect an existing private bucket, or follow the setup instructions to create one."
				def = "existing"
				options = options[1:]
			}
		}
		terminal.Println(p.out, explanation)
		choice, err := p.actions(title, def, nil, options)
		if err != nil {
			return "", err
		}
		if choice == "help" {
			storageHelp(p)
			continue
		}
		return choice, nil
	}
}

func promptExistingR2(p *prompter, cfg credentials.Config, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	var secret credentials.R2Credentials
	var err error
	previous := cfg
	// The account comes first, so a pasted bucket URL can fill in the
	// bucket too.
	fromURL, e := promptR2Location(p, &cfg)
	if e != nil {
		return cfg, secret, false, e
	}
	if !fromURL {
		if cfg.Bucket, err = p.required("Bucket name", cfg.Bucket); err != nil {
			return cfg, secret, false, err
		}
	}
	if cfg.Bucket != previous.Bucket || cfg.R2Endpoint != previous.R2Endpoint || cfg.R2AccountID != previous.R2AccountID {
		cfg.R2CredentialRef = ""
	}
	reuse := false
	if cfg.R2CredentialRef != "" {
		// A rebuilt or reinstalled binary can lose access to the item it
		// stored; offering to keep it would only fail after the
		// questions, so ask for the key again right away.
		if storedCredentialReadable(env, cfg.R2CredentialRef) {
			reuse, err = p.yesNo("Keep stored R2 credentials?", true)
			if err != nil {
				return cfg, secret, false, err
			}
		} else {
			terminal.Println(p.out, "The stored R2 credentials can't be read from the "+credentials.StoreName(credentialOS)+"; enter them again.")
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

	cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)
	return cfg, secret, secret.SecretAccessKey != "", err
}
