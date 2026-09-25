package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// collectorAWSFiles are the AWS SDK variables that name the files an S3
// profile is read from. Setup reads profiles through them and runs its
// storage check with them, so the collector's LaunchAgent records them.
// Credential variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
// AWS_SESSION_TOKEN) are never recorded: the plist is not a place for
// secrets, and the configured profile, not the shell, is what supplies
// credentials.
var collectorAWSFiles = []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"}

// launchdPath is the PATH launchd gives a job whose plist sets none.
const launchdPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// collectorEnvironment is what the collector's LaunchAgent sets besides
// AGENT_ARCHIVE_HOME, so a scheduled pass loads storage credentials the way
// setup's storage check just did. launchd starts the job with none of the
// shell's environment.
//
// For S3 it records the AWS file variables that are set, as absolute paths,
// and always a PATH: this shell's absolute entries followed by launchd's
// own. PATH is recorded for every S3 profile, not only one that uses
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
	for _, name := range collectorAWSFiles {
		if value, ok := e.lookupEnv(name); ok && strings.TrimSpace(value) != "" {
			environment[name] = e.absolutePath(strings.TrimSpace(value))
		}
	}
	shellPath, _ := e.lookupEnv("PATH")
	environment["PATH"] = collectorPath(shellPath)
	return environment
}

// collectorPath is shellPath's absolute entries, without repeats, followed
// by those of launchd's default PATH it lacks. A relative entry would be
// resolved against the collector's working directory, not setup's.
func collectorPath(shellPath string) string {
	var entries []string
	for _, entry := range append(filepath.SplitList(shellPath), filepath.SplitList(launchdPath)...) {
		if filepath.IsAbs(entry) && !slices.Contains(entries, filepath.Clean(entry)) {
			entries = append(entries, filepath.Clean(entry))
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// absolutePath resolves path against the working directory, as the AWS SDK
// does for a relative AWS_CONFIG_FILE; the collector runs elsewhere.
func (e Env) absolutePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
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
