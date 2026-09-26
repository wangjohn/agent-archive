package cli

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// collectorAWSFiles are the AWS SDK variables that name files an S3 pass
// reads: the profile's config and credentials files, and the CA bundle a
// TLS-inspecting network needs. Setup reads profiles through them and runs
// its storage check with them, so the collector's LaunchAgent records them.
// Credential variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
// AWS_SESSION_TOKEN) are never recorded: the plist is not a place for
// secrets, and the configured profile, not the shell, is what supplies
// credentials.
var collectorAWSFiles = []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CA_BUNDLE"}

// collectorAWSEndpoints are the endpoint overrides for the services an S3
// pass can call: S3 itself, and STS, SSO and SSO OIDC while the profile's
// credentials are resolved. They are addresses, not secrets; one that
// carries a user name or password in its URL is not recorded.
var collectorAWSEndpoints = []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL_SSO", "AWS_ENDPOINT_URL_SSO_OIDC"}

// collectorProxies are the proxy variables Go's HTTP client reads, in both
// spellings: a network that only lets traffic out through a proxy needs
// them for every S3 request and credential exchange. A proxy URL with a
// user name or password in it is not recorded (see
// collectorEnvironmentLeftOut).
var collectorProxies = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// collectorHelperSettings are credential_process helpers' own settings
// that say where credentials live or how to ask for them: aws-vault's
// backend, keychain, prompt and pass prefix, and 1Password's account.
// Each is reviewed by name, never matched by prefix, because the same
// helpers keep secrets in variables of the same family
// (AWS_VAULT_FILE_PASSPHRASE, OP_SERVICE_ACCOUNT_TOKEN, OP_SESSION_*),
// which must never reach the plist.
var collectorHelperSettings = []string{"AWS_VAULT_BACKEND", "AWS_VAULT_KEYCHAIN_NAME", "AWS_VAULT_PROMPT", "AWS_VAULT_PASS_PREFIX", "OP_ACCOUNT"}

// collectorHelperDirs are the helpers' settings that name a directory,
// recorded as absolute paths like collectorAWSFiles: aws-vault's file
// backend and 1Password's configuration.
var collectorHelperDirs = []string{"AWS_VAULT_FILE_DIR", "OP_CONFIG_DIR"}

// launchdPath is the PATH launchd gives a job whose plist sets none.
const launchdPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// collectorEnvironment is what the collector's LaunchAgent sets besides
// AGENT_ARCHIVE_HOME, so a scheduled pass loads storage credentials the way
// setup's storage check just did. launchd starts the job with none of the
// shell's environment.
//
// For S3 it records the AWS file variables that are set, as absolute paths,
// the endpoint overrides, proxy settings and reviewed helper settings that
// are set, and always a PATH: this shell's usable entries followed by launchd's own. PATH is
// recorded for every S3 profile, not only one that uses
// credential_process today, because the SDK can reach a credential_process
// through a source_profile chain, the command it runs (aws-vault, 1Password's
// op, granted) runs helpers of its own through PATH, and a profile can gain a
// credential_process after setup without setup running again. A PATH names
// directories, never secrets. R2's credentials come from the Keychain in
// process, so an R2 collector needs nothing more.
func (e Env) collectorEnvironment(storage credentials.Config) map[string]string {
	if storage.Provider != credentials.ProviderS3 {
		return nil
	}
	environment := map[string]string{}
	for _, name := range slices.Concat(collectorAWSFiles, collectorHelperDirs) {
		if value, ok := e.lookupEnv(name); ok && strings.TrimSpace(value) != "" {
			environment[name] = e.absolutePath(strings.TrimSpace(value))
		}
	}
	for _, name := range collectorHelperSettings {
		if value, _ := e.lookupEnv(name); strings.TrimSpace(value) != "" {
			environment[name] = strings.TrimSpace(value)
		}
	}
	for _, name := range slices.Concat(collectorAWSEndpoints, collectorProxies) {
		value, _ := e.lookupEnv(name)
		if value = strings.TrimSpace(value); value != "" && !carriesCredentials(value) {
			environment[name] = value
		}
	}
	shellPath, _ := e.lookupEnv("PATH")
	environment["PATH"] = collectorPath(shellPath)
	return environment
}

// collectorEnvironmentLeftOut names the endpoint and proxy variables set in
// this shell that collectorEnvironment does not record because their URL
// carries a user name or password: the collector runs without them, so
// setup says so.
func (e Env) collectorEnvironmentLeftOut(storage credentials.Config) []string {
	if storage.Provider != credentials.ProviderS3 {
		return nil
	}
	var left []string
	for _, name := range slices.Concat(collectorAWSEndpoints, collectorProxies) {
		if value, _ := e.lookupEnv(name); carriesCredentials(strings.TrimSpace(value)) {
			left = append(left, name)
		}
	}
	return left
}

// carriesCredentials reports whether an endpoint or proxy setting holds a
// user name or password: a URL's user information, or an @ in a bare
// host:port, which Go's proxy handling reads the same way.
func carriesCredentials(value string) bool {
	if strings.Contains(value, "@") {
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.User != nil
}

// collectorPath is shellPath's usable entries, without repeats, followed by
// those of launchd's default PATH it lacks. An entry is left out when it is
// relative (it would be resolved against the collector's working
// directory, not setup's), is not a directory, or is writable by every
// account: the LaunchAgent runs every minute, so a program another account
// planted there would run as this one.
func collectorPath(shellPath string) string {
	var entries []string
	for _, entry := range filepath.SplitList(shellPath) {
		entry = filepath.Clean(entry)
		if !filepath.IsAbs(entry) || slices.Contains(entries, entry) {
			continue
		}
		if info, err := os.Stat(entry); err != nil || !info.IsDir() || info.Mode().Perm()&0o002 != 0 {
			continue
		}
		entries = append(entries, entry)
	}
	for _, entry := range filepath.SplitList(launchdPath) {
		if !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// absolutePath resolves path against the working directory, as the AWS SDK
// does for a relative AWS_CONFIG_FILE; the collector runs elsewhere. A path
// starting with ~ is kept as written: the program reading it expands it
// against the home directory, which the collector shares.
func (e Env) absolutePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if strings.HasPrefix(path, "~") {
		return path
	}
	dir, err := os.Getwd()
	if e.WorkingDir != nil {
		dir, err = e.WorkingDir()
	}
	if err != nil {
		return path
	}
	return filepath.Join(dir, path)
}

// awsFiles are the shared config and credentials files the AWS SDK reads
// under environment, a lookup like Env.lookupEnv.
func awsFiles(userHome string, environment func(string) (string, bool)) (configFile, credentialsFile string) {
	value := func(name string) string {
		v, _ := environment(name)
		return strings.TrimSpace(v)
	}
	return firstNonEmpty(value("AWS_CONFIG_FILE"), filepath.Join(userHome, ".aws", "config")),
		firstNonEmpty(value("AWS_SHARED_CREDENTIALS_FILE"), filepath.Join(userHome, ".aws", "credentials"))
}

// collectorEnvironmentProblems says why a collector running with
// environment (a LaunchAgent's EnvironmentVariables) cannot load storage's
// S3 profile, as far as can be told without running anything: an AWS file
// variable naming a file that is gone, or a credential_process whose
// program is not on the collector's PATH. It reads profile settings only;
// it never runs credential_process or retrieves credentials.
func collectorEnvironmentProblems(storage credentials.Config, environment map[string]string, userHome string) []string {
	if storage.Provider != credentials.ProviderS3 || storage.AWSProfile == "" {
		return nil
	}
	lookup := func(name string) (string, bool) { v, ok := environment[name]; return v, ok }
	var problems []string
	for _, name := range collectorAWSFiles {
		if path := environment[name]; path != "" {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				problems = append(problems, fmt.Sprintf("The background collector reads AWS profiles from %s (%s), which does not exist.", path, name))
			}
		}
	}
	configFile, credentialsFile := awsFiles(userHome, lookup)
	command := credentialProcess(configFile, credentialsFile, storage.AWSProfile)
	program := commandProgram(command)
	if program == "" {
		return problems
	}
	path := cmp.Or(environment["PATH"], launchdPath)
	if !programFound(program, path, userHome) {
		problems = append(problems, fmt.Sprintf("AWS profile %q gets its credentials by running %s, which the background collector cannot find on its PATH (%s).", storage.AWSProfile, program, path))
	}
	return problems
}

// credentialProcess is the credential_process the SDK would run for
// profile, following source_profile, or "" when it runs none or the profile
// cannot be read (the storage check reports that more usefully).
func credentialProcess(configFile, credentialsFile, profile string) string {
	cfg, err := awsconfig.LoadSharedConfigProfile(context.Background(), profile, func(o *awsconfig.LoadSharedConfigOptions) {
		o.ConfigFiles = []string{configFile}
		o.CredentialsFiles = []string{credentialsFile}
	})
	if err != nil {
		return ""
	}
	for profile := &cfg; profile != nil; profile = profile.Source {
		if profile.CredentialProcess != "" {
			return profile.CredentialProcess
		}
	}
	return ""
}

// commandProgram is the program a credential_process command line runs
// (the SDK passes it to sh -c): its first word, unquoted. It is "" when the
// command starts with something only the shell can resolve, such as a
// variable assignment or expansion, so nothing is reported that might be
// wrong.
func commandProgram(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	var word string
	if quote := command[0]; quote == '"' || quote == '\'' {
		end := strings.IndexByte(command[1:], quote)
		if end < 0 {
			return ""
		}
		word = command[1 : end+1]
	} else {
		word, _, _ = strings.Cut(command, " ")
		word, _, _ = strings.Cut(word, "\t")
		if strings.ContainsAny(word, "=$`;|&<>(){}\\\"'*?[") {
			return ""
		}
	}
	return word
}

// programFound reports whether sh would find program with PATH set to path:
// a name is looked up in each directory, a path is taken as it is (from the
// root directory, the collector's working directory, when relative), and a
// leading ~/ is the user's home.
func programFound(program, path, userHome string) bool {
	if rest, ok := strings.CutPrefix(program, "~/"); ok {
		program = filepath.Join(userHome, rest)
	}
	if strings.Contains(program, "/") {
		if !filepath.IsAbs(program) {
			program = string(filepath.Separator) + program
		}
		return executableProblem(filepath.Clean(program)) == ""
	}
	for _, dir := range filepath.SplitList(path) {
		if filepath.IsAbs(dir) && executableProblem(filepath.Join(dir, program)) == "" {
			return true
		}
	}
	return false
}

// awsFilesDrift says when commands run from this shell read S3 profiles
// from other files than the collector does, so sync and the collector could
// disagree; "" when they read the same ones.
func (e Env) awsFilesDrift(storage credentials.Config, environment map[string]string, userHome string) string {
	if storage.Provider != credentials.ProviderS3 {
		return ""
	}
	collectorConfig, collectorCredentials := awsFiles(userHome, func(name string) (string, bool) { v, ok := environment[name]; return v, ok })
	shellConfig, shellCredentials := awsFiles(userHome, func(name string) (string, bool) {
		v, ok := e.lookupEnv(name)
		if strings.TrimSpace(v) == "" {
			return "", false
		}
		return e.absolutePath(strings.TrimSpace(v)), ok
	})
	var differ []string
	if filepath.Clean(shellConfig) != filepath.Clean(collectorConfig) {
		differ = append(differ, fmt.Sprintf("config %s here, %s for the collector", shellConfig, collectorConfig))
	}
	if filepath.Clean(shellCredentials) != filepath.Clean(collectorCredentials) {
		differ = append(differ, fmt.Sprintf("credentials %s here, %s for the collector", shellCredentials, collectorCredentials))
	}
	if len(differ) == 0 {
		return ""
	}
	return fmt.Sprintf("This shell's AWS settings files differ from the background collector's (%s). The collector uses the ones setup verified; if this shell's are the right ones, run agent-archive setup again from here.", strings.Join(differ, "; "))
}

// warnCollectorEnvironment says, before setup commits, why the collector it
// is about to install could not load storage's profile (see
// collectorEnvironmentProblems).
func warnCollectorEnvironment(p *prompter, storage credentials.Config, userHome string, env Env) {
	for _, problem := range collectorEnvironmentProblems(storage, env.collectorEnvironment(storage), userHome) {
		p.warn(problem, "Scheduled uploads will fail until it can; agent-archive sync from this shell still works. Run setup from a shell where the profile works without aliases or shell functions.")
	}
	if left := env.collectorEnvironmentLeftOut(storage); len(left) > 0 {
		p.warn(strings.Join(left, ", ")+" holds a user name or password, so the background collector runs without it.", "If the network needs it, scheduled uploads will fail while agent-archive sync from this shell still works. Use a proxy or endpoint that needs no credentials in its URL.")
	}
}
