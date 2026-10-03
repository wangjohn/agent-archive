package cli

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// These substitutes deliberately do not embed Env. They keep the preflight
// and collector environment builders callable with only their actual inputs.
type preflightProbe struct {
	files hooks.Files
	job   scheduler.JobState
	open  func() (credentials.CredentialStore, error)
}

func (p preflightProbe) hookFiles(string) hooks.Files { return p.files }

func (p preflightProbe) installation(home, userHome string) installation {
	return installation{home: home, userHome: userHome, accountHome: userHome, sched: func() scheduler.Scheduler { return launchd.Scheduler{} }}
}

func (p preflightProbe) jobStatus(string, scheduler.Ref) scheduler.Status {
	return scheduler.Status{State: p.job, Problem: &scheduler.Problem{Kind: scheduler.ProblemCannotTell}}
}

func (p preflightProbe) credentialStore() (credentials.CredentialStore, error) {
	return p.open()
}

func TestPreflightUsesOnlyItsDependencies(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	probe := preflightProbe{
		files: hooks.Files{},
		job:   scheduler.Unknown,
		open: func() (credentials.CredentialStore, error) {
			return nil, credentials.ErrKeychainLocked
		},
	}
	checks := preflight(probe, home, userHome, preflightScope{r2: true})
	if len(checks) != 2 || checks[0].Label != "Background job" || checks[0].OK || checks[1].Label != "Keychain" || checks[1].OK {
		t.Fatalf("preflight checks = %+v", checks)
	}
	if !checks.blocked() {
		t.Fatal("failed dependency checks did not block setup")
	}
}

type collectorEnvironmentProbe struct {
	values map[string]string
	base   string
	system platform.OS
	reads  int
}

func (p *collectorEnvironmentProbe) operatingSystem() platform.OS { return p.system }

func (p *collectorEnvironmentProbe) lookupEnv(name string) (string, bool) {
	p.reads++
	v, ok := p.values[name]
	return v, ok
}

func (p *collectorEnvironmentProbe) defaultPATH() string { return launchd.DefaultPATH }

func (p *collectorEnvironmentProbe) absolutePath(path string) string {
	return filepath.Join(p.base, path)
}

func TestCollectorEnvironmentBuilderUsesOnlyItsSource(t *testing.T) {
	t.Parallel()
	probe := &collectorEnvironmentProbe{
		values: map[string]string{
			"AWS_CONFIG_FILE":     "config/custom",
			"AWS_ENDPOINT_URL_S3": "https://user:secret@s3.example.test",
			"PATH":                "/usr/bin",
		},
		base:   t.TempDir(),
		system: platform.Darwin,
	}
	storage := credentials.Config{Provider: credentials.ProviderS3}
	got := buildCollectorEnvironment(probe, storage)
	if got["AWS_CONFIG_FILE"] != filepath.Join(probe.base, "config", "custom") {
		t.Fatalf("AWS config path = %q", got["AWS_CONFIG_FILE"])
	}
	if _, ok := got["AWS_ENDPOINT_URL_S3"]; ok {
		t.Fatal("collector environment recorded credentials in an endpoint URL")
	}
	if got["PATH"] == "" || !slices.Contains(collectorEnvironmentOmissions(probe, storage), "AWS_ENDPOINT_URL_S3") {
		t.Fatalf("collector environment = %v, omissions = %v", got, collectorEnvironmentOmissions(probe, storage))
	}
	reads := probe.reads
	if got := buildCollectorEnvironment(probe, credentials.Config{Provider: credentials.ProviderR2}); got != nil || probe.reads != reads {
		t.Fatalf("R2 collector read shell environment: %v", got)
	}
}
