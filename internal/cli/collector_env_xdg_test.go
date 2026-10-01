package cli

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// xdgProbe is a collectorEnvironmentSource for a system and a shell.
func xdgProbe(system platform.OS, values map[string]string) *collectorEnvironmentProbe {
	return &collectorEnvironmentProbe{values: values, base: "/work", system: system}
}

// On Linux the job gets the shell's XDG_CONFIG_HOME and XDG_CACHE_HOME, for
// every storage provider (Cursor's database and its temporary copies are where
// they say, whatever the bucket is), when they are absolute; a relative or
// empty one is invalid to the XDG specification and is left out, as it is to
// every program that reads it.
func TestLinuxJobGetsTheShellsXDGDirectories(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   map[string]string
	}{
		{"both set", map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"}, map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"}},
		{"one set", map[string]string{"XDG_CACHE_HOME": "/srv/cache"}, map[string]string{"XDG_CACHE_HOME": "/srv/cache"}},
		{"recorded cleaned", map[string]string{"XDG_CONFIG_HOME": "/srv/config/"}, map[string]string{"XDG_CONFIG_HOME": "/srv/config"}},
		{"relative", map[string]string{"XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "./cache"}, nil},
		{"tilde", map[string]string{"XDG_CONFIG_HOME": "~/config"}, nil},
		{"empty", map[string]string{"XDG_CONFIG_HOME": "", "XDG_CACHE_HOME": ""}, nil},
		{"unset", map[string]string{}, nil},
	} {
		got := buildCollectorEnvironment(xdgProbe(platform.Linux, tc.values), credentials.Config{Provider: credentials.ProviderR2})
		if len(got) != len(tc.want) {
			t.Errorf("%s, R2: %v, want %v", tc.name, got, tc.want)
		}
		for name, want := range tc.want {
			if got[name] != want {
				t.Errorf("%s, R2: %s = %q, want %q", tc.name, name, got[name], want)
			}
		}
		s3 := buildCollectorEnvironment(xdgProbe(platform.Linux, tc.values), credentials.Config{Provider: credentials.ProviderS3})
		for _, name := range collectorXDGDirs {
			if s3[name] != tc.want[name] {
				t.Errorf("%s, S3: %s = %q, want %q", tc.name, name, s3[name], tc.want[name])
			}
		}
		if s3["PATH"] == "" {
			t.Errorf("%s, S3: no PATH in %v", tc.name, s3)
		}
	}
}

// macOS never records them, whatever the provider: Cursor's data there does
// not follow XDG, and a plist that gained a variable would no longer be
// byte-identical. An unknown system does not either.
func TestOtherSystemsJobsNeverGetXDGDirectories(t *testing.T) {
	t.Parallel()
	shell := map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"}
	for _, system := range []platform.OS{platform.Darwin, platform.Unknown} {
		for _, provider := range []string{credentials.ProviderR2, credentials.ProviderS3} {
			got := buildCollectorEnvironment(xdgProbe(system, shell), credentials.Config{Provider: provider})
			for _, name := range collectorXDGDirs {
				if _, ok := got[name]; ok {
					t.Errorf("%s, %s: recorded %s in %v", system, provider, name, got)
				}
			}
		}
	}
	if got := buildCollectorEnvironment(xdgProbe(platform.Darwin, shell), credentials.Config{Provider: credentials.ProviderR2}); got != nil {
		t.Errorf("macOS R2 environment %v, want none", got)
	}
}

// Setup on Linux records the shell's XDG directories in the unit, and status
// says when this shell's differ from the collector's afterwards: for a
// variable the shell changed, set, or dropped since setup, and for a value
// that was never recorded. A shell that still agrees, and one whose value the
// XDG specification ignores, hear nothing.
func TestStatusWarnsWhenTheShellsXDGDirectoriesDriftFromTheCollectors(t *testing.T) {
	t.Parallel()
	recorded := map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"}
	for _, tc := range []struct {
		name  string
		shell map[string]string
		warns []string // each a variable that should be named
	}{
		{"unchanged", recorded, nil},
		{"spelled differently but the same", map[string]string{"XDG_CONFIG_HOME": "/srv/config/", "XDG_CACHE_HOME": "/srv//cache"}, nil},
		{"config home changed", map[string]string{"XDG_CONFIG_HOME": "/elsewhere", "XDG_CACHE_HOME": "/srv/cache"}, []string{"XDG_CONFIG_HOME"}},
		{"cache home dropped", map[string]string{"XDG_CONFIG_HOME": "/srv/config"}, []string{"XDG_CACHE_HOME"}},
		{"both dropped", map[string]string{}, []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"}},
		{"relative is unset", map[string]string{"XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "/srv/cache"}, []string{"XDG_CONFIG_HOME"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newLinuxInstall(t)
			l.env.LookupEnv = shellEnvironment(recorded)
			l.setup()
			ref := l.env.installation(l.home, l.userHome).ref()
			if got := l.env.jobDefinition(l.userHome, ref).Env; got["XDG_CONFIG_HOME"] != "/srv/config" || got["XDG_CACHE_HOME"] != "/srv/cache" {
				t.Fatalf("%s: the unit records %v, want the shell's XDG directories", tc.name, got)
			}
			l.env.LookupEnv = shellEnvironment(tc.shell)
			warnings := statusWarnings(t, l.env)
			for _, name := range collectorXDGDirs {
				mentioned := false
				for _, w := range warnings {
					mentioned = mentioned || strings.Contains(w, "This shell's "+name+" ")
				}
				if want := slices.Contains(tc.warns, name); mentioned != want {
					t.Errorf("%s: warning about %s = %v, want %v (warnings %q)", tc.name, name, mentioned, want, warnings)
				}
			}
		})
	}
}

// A collector set up before its shell had the variable (or by a build that did
// not record it) has none: status says so when the shell has one now, and
// says nothing while the shell has none either.
func TestStatusWarnsWhenTheCollectorHasNoXDGDirectoryTheShellHas(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.LookupEnv = shellEnvironment(map[string]string{})
	l.setup()
	if warnings := statusWarnings(t, l.env); len(warnings) != 0 {
		t.Fatalf("no XDG directory anywhere: warnings %q", warnings)
	}
	l.env.LookupEnv = shellEnvironment(map[string]string{"XDG_CACHE_HOME": "/srv/cache"})
	warnings := statusWarnings(t, l.env)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "This shell's XDG_CACHE_HOME is /srv/cache and the background collector's is not set") || !strings.Contains(warnings[0], "agent-archive setup") {
		t.Errorf("warnings %q", warnings)
	}
}

// The warning's words for each case: both values, what an unset one means,
// a value the specification ignores shown as the shell has it (so the user
// recognizes it) and why it counts as unset, and the fix.
func TestXDGDriftWarningSaysWhatEachSideHas(t *testing.T) {
	t.Parallel()
	const fix = "The collector uses what setup recorded; if this shell's is the right one, run agent-archive setup again from here."
	for _, tc := range []struct {
		name      string
		shell     map[string]string
		collector map[string]string
		want      []string
	}{
		{"changed", map[string]string{"XDG_CACHE_HOME": "/elsewhere"}, map[string]string{"XDG_CACHE_HOME": "/srv/cache"},
			[]string{"This shell's XDG_CACHE_HOME is /elsewhere and the background collector's is /srv/cache, so they keep the temporary copies they read Cursor's chat database from in different places. " + fix}},
		{"dropped", map[string]string{}, map[string]string{"XDG_CONFIG_HOME": "/srv/config"},
			[]string{"This shell's XDG_CONFIG_HOME is not set (the default under the home directory) and the background collector's is /srv/config, so they look for Cursor's chat database in different places. " + fix}},
		{"relative", map[string]string{"XDG_CONFIG_HOME": "config"}, map[string]string{"XDG_CONFIG_HOME": "/srv/config"},
			[]string{`This shell's XDG_CONFIG_HOME is "config", which is not an absolute path and so counts as not set (the default under the home directory) and the background collector's is /srv/config, so they look for Cursor's chat database in different places. ` + fix}},
		{"relative on both sides", map[string]string{"XDG_CONFIG_HOME": "config"}, map[string]string{}, nil},
	} {
		env := Env{OS: platform.Linux, LookupEnv: shellEnvironment(tc.shell)}
		if got := env.xdgDrift(tc.collector); !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		env.OS = platform.Darwin
		if got := env.xdgDrift(tc.collector); got != nil {
			t.Errorf("%s on macOS: %q", tc.name, got)
		}
	}
}

// The same holds for R2, whose job had no environment of its own before: the
// real setup writes the shell's XDG directories into the unit, setup
// --refresh from a shell that now has others keeps what setup recorded (as it
// keeps the AWS variables), and status then says the two differ.
func TestLinuxR2SetupRecordsXDGDirectoriesAndRefreshKeepsThem(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	recorded := map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache/"}
	l.env.LookupEnv = shellEnvironment(recorded)
	setupRun(t, l.env, r2SetupInput(t.TempDir(), "r2-secret"), 0)
	ref := l.env.installation(l.home, l.userHome).ref()
	want := map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"}
	if got := l.env.jobDefinition(l.userHome, ref).Env; !maps.Equal(got, want) {
		t.Fatalf("the R2 unit records %v, want %v", got, want)
	}
	if warnings := statusWarnings(t, l.env); len(warnings) != 0 {
		t.Fatalf("status right after setup warns %q", warnings)
	}
	l.env.LookupEnv = shellEnvironment(map[string]string{"XDG_CACHE_HOME": "/elsewhere"})
	if code, stdout, stderr := refreshRun(t, l.env); code != 0 {
		t.Fatalf("refresh: exit %d\n%s%s", code, stdout, stderr)
	}
	if got := l.env.jobDefinition(l.userHome, ref).Env; !maps.Equal(got, want) {
		t.Errorf("after refresh the unit records %v, want what setup recorded, %v", got, want)
	}
	warnings := statusWarnings(t, l.env)
	if len(warnings) != 2 || !strings.HasPrefix(warnings[0], "This shell's XDG_CONFIG_HOME is not set") || !strings.HasPrefix(warnings[1], "This shell's XDG_CACHE_HOME is /elsewhere and the background collector's is /srv/cache") {
		t.Errorf("warnings %q", warnings)
	}
}

// On macOS the collector records no XDG variable, so a shell that sets one is
// never told it differs.
func TestStatusOnMacOSIgnoresXDGDirectories(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LookupEnv = shellEnvironment(map[string]string{"XDG_CONFIG_HOME": "/srv/config", "XDG_CACHE_HOME": "/srv/cache"})
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	environment, _ := collectorPlistEnvironment(t, env, home, userHome)
	for _, name := range collectorXDGDirs {
		if _, ok := environment[name]; ok {
			t.Errorf("the plist records %s: %v", name, environment)
		}
	}
	if warnings := statusWarnings(t, env); len(warnings) != 0 {
		t.Errorf("status warnings on macOS: %q", warnings)
	}
}

// statusWarnings is what status --json lists under warnings.
func statusWarnings(t *testing.T, env Env) []string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, &out, &errOut)
	}
	var status struct {
		Warnings []string `json:"warnings"`
	}
	must(t, json.Unmarshal(out.Bytes(), &status))
	return status.Warnings
}
