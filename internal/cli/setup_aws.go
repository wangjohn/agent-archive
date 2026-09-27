package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// AWSProfile contains only the information needed to offer a profile in setup.
// Discovery never retrieves credentials, runs credential_process, or logs in.
type AWSProfile struct {
	Name   string
	Region string
	// NoCredentials is true when the profile's settings name no credential
	// source (no access keys, credential_process, SSO, login session, or
	// role with a source to assume it from) or the SDK cannot load it.
	// Discovery checks only that such a setting is present.
	NoCredentials bool
}

func (e Env) awsProfiles() ([]AWSProfile, error) {
	if e.AWSProfiles != nil {
		return e.AWSProfiles()
	}
	home, err := e.userHomeDir()
	if err != nil {
		return nil, err
	}
	// The same files the collector's LaunchAgent is given (see
	// collectorEnvironment).
	return readAWSProfiles(awsFiles(home, e.lookupEnv))
}

func readAWSProfiles(configPath, credentialsPath string) ([]AWSProfile, error) {
	names := map[string]bool{}
	for i, path := range []string{configPath, credentialsPath} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// This scan uses only section names. The SDK parses the values below, but
			// discovery keeps only the region and whether a credential setting is present.
			if !strings.HasPrefix(line, "[") {
				continue
			}
			end := strings.Index(line, "]")
			if end < 0 {
				continue
			}
			name := strings.TrimSpace(line[1:end])
			if i == 0 {
				if name != "default" && !strings.HasPrefix(name, "profile ") {
					continue
				}
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			}
			if name != "" {
				names[name] = true
			}
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
	}
	profiles := make([]AWSProfile, 0, len(names))
	for name := range names {
		cfg, err := awsconfig.LoadSharedConfigProfile(context.Background(), name, func(o *awsconfig.LoadSharedConfigOptions) {
			o.ConfigFiles = []string{configPath}
			o.CredentialsFiles = []string{credentialsPath}
		})
		if err != nil {
			// The SDK cannot load this profile (a broken source_profile
			// chain, a missing sso-session section, conflicting credential
			// settings), so it cannot supply credentials either.
			profiles = append(profiles, AWSProfile{Name: name, NoCredentials: true})
			continue
		}
		profiles = append(profiles, AWSProfile{Name: name, Region: cfg.Region, NoCredentials: !hasCredentialSource(cfg)})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// hasCredentialSource reports whether a profile names any way to get
// credentials. It looks only at which settings are present; nothing is
// retrieved or run. A role counts only with something to assume it from.
func hasCredentialSource(cfg awsconfig.SharedConfig) bool {
	role := cfg.RoleARN != "" && (cfg.SourceProfileName != "" || cfg.CredentialSource != "" || cfg.WebIdentityTokenFile != "")
	return cfg.Credentials.HasKeys() || cfg.CredentialProcess != "" || cfg.WebIdentityTokenFile != "" ||
		role || cfg.SSOSessionName != "" || cfg.SSOStartURL != "" || cfg.LoginSession != ""
}

// usableAWSProfiles names the discovered profiles that have a credential
// source.
func usableAWSProfiles(profiles []AWSProfile) []string {
	var names []string
	for _, profile := range profiles {
		if !profile.NoCredentials {
			names = append(names, profile.Name)
		}
	}
	return names
}

// defaultAWSProfile is the profile setup offers first: the saved one, then
// AWS_PROFILE, then "default" or the only profile, if it has a credential
// source.
func defaultAWSProfile(saved string, profiles []AWSProfile, env Env) string {
	if saved != "" {
		return saved
	}
	if name := lookupEnvTrimmed(env, "AWS_PROFILE"); name != "" {
		return name
	}
	usable := usableAWSProfiles(profiles)
	if containsString(usable, "default") {
		return "default"
	}
	if len(usable) == 1 {
		return usable[0]
	}
	return ""
}

func promptAWSProfile(p *prompter, cfg *credentials.Config, env Env) error {
	profiles, err := env.awsProfiles()
	if err != nil {
		terminal.Println(p.out, "Could not read AWS profiles automatically. Enter an existing profile name below.")
	}
	def := defaultAWSProfile(cfg.AWSProfile, profiles, env)
	var profile string
	if len(profiles) > 0 {
		profile, err = pickAWSProfile(p, profiles, def)
	} else {
		profile, err = p.required("AWS profile", def)
	}
	if err != nil {
		return err
	}
	if profile != cfg.AWSProfile {
		cfg.Region = ""
	}
	cfg.AWSProfile = profile
	if cfg.Region == "" {
		for _, candidate := range profiles {
			if candidate.Name == profile {
				cfg.Region = candidate.Region
				break
			}
		}
	}
	if cfg.Region == "" {
		cfg.Region, err = p.required("Bucket region (for example us-east-1)", "")
		return err
	}
	terminal.Printf(p.out, "Using region %s. You can change it at the final review.\n", cfg.Region)
	return nil
}

// pickAWSProfile lists the discovered profiles by number, marking those with
// no credential source. A profile that discovery missed can still be typed
// by name.
func pickAWSProfile(p *prompter, profiles []AWSProfile, def string) (string, error) {
	terminal.Println(p.out, "Which AWS profile has access to the bucket?")
	defNum := def
	for i, profile := range profiles {
		note := ""
		if profile.NoCredentials {
			note = " (no credentials configured)"
		}
		terminal.Printf(p.out, "  %d) %s%s\n", i+1, profile.Name, note)
		if profile.Name == def {
			defNum = strconv.Itoa(i + 1)
		}
	}
	label := fmt.Sprintf("Enter 1-%d, or another profile name", len(profiles))
	for {
		answer, err := p.withDefault(label, defNum)
		if err != nil {
			return "", err
		}
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(profiles) {
			return profiles[n-1].Name, nil
		}
		if answer != "" {
			return answer, nil
		}
		terminal.Println(p.out, "This value is required.")
	}
}
