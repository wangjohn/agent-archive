package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestAWSProfileDiscoveryReadsSettingsWithoutRunningCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	credsPath := filepath.Join(dir, "credentials")
	marker := filepath.Join(dir, "should-not-exist")
	data := "[default]\nregion = us-east-1\n[profile work]\nregion = eu-west-1\ncredential_process = touch " + marker + "\n[sso-session company]\nsso_region = us-west-2\n" +
		"[profile sso]\nsso_session = company\nsso_account_id = 111111111111\nsso_role_name = Archive\n" +
		"[profile role]\nrole_arn = arn:aws:iam::111111111111:role/archive\nsource_profile = legacy\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsPath, []byte("[legacy]\naws_access_key_id = synthetic\naws_secret_access_key = synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	profiles, err := (Env{}).awsProfiles()
	want := []AWSProfile{
		{Name: "default", Region: "us-east-1", NoCredentials: true},
		{Name: "legacy"},
		{Name: "role"},
		{Name: "sso"},
		{Name: "work", Region: "eu-west-1"},
	}
	if err != nil || !reflect.DeepEqual(profiles, want) {
		t.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("credential process executed")
	}
}

func TestAWSProfileDiscoveryMarksProfilesThatCannotSupplyCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	data := "[profile base]\nregion = us-east-1\n" +
		// Assumes a role from a profile that has no credentials.
		"[profile chained]\nrole_arn = arn:aws:iam::111111111111:role/archive\nsource_profile = base\n" +
		// A role with nothing to assume it from.
		"[profile lonerole]\nrole_arn = arn:aws:iam::111111111111:role/archive\n" +
		// Names an sso-session section that does not exist.
		"[profile badsso]\nsso_session = missing\n" +
		"[profile webid]\nrole_arn = arn:aws:iam::111111111111:role/archive\nweb_identity_token_file = /nonexistent/token\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	profiles, err := readAWSProfiles(configPath, filepath.Join(dir, "credentials"))
	want := []AWSProfile{
		{Name: "badsso", NoCredentials: true},
		{Name: "base", Region: "us-east-1", NoCredentials: true},
		{Name: "chained", NoCredentials: true},
		{Name: "lonerole", NoCredentials: true},
		{Name: "webid"},
	}
	if err != nil || !reflect.DeepEqual(profiles, want) {
		t.Fatalf("profiles=%+v err=%v", profiles, err)
	}
}

func TestStorageProviderDefaultSkipsDiscoveryWhenProviderSaved(t *testing.T) {
	t.Parallel()
	called := false
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { called = true; return nil, nil }}
	var out bytes.Buffer
	_, _, _, _ = promptStorage(newPrompter(strings.NewReader(""), &out), credentials.Config{Provider: credentials.ProviderS3}, env)
	if called {
		t.Fatal("AWS profile discovery ran although a provider was saved")
	}
	if !strings.Contains(out.String(), "Enter 1-3 [2]: ") {
		t.Fatalf("output %q, want the saved S3 as default", &out)
	}
}

func TestAWSProfileSwitchDoesNotReuseOldRegion(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{AWSProfile: "old", Region: "us-east-1"}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "new"}}, nil }}
	var out bytes.Buffer
	err := promptAWSProfile(newPrompter(strings.NewReader("new\neu-west-1\n"), &out), &cfg, env)
	if err != nil || cfg.Region != "eu-west-1" || cfg.AWSProfile != "new" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestPickAWSProfileByNumberOrName(t *testing.T) {
	t.Parallel()
	profiles := []AWSProfile{{Name: "default"}, {Name: "work"}}
	for input, want := range map[string]string{"2\n": "work", "\n": "default", "other\n": "other"} {
		var out bytes.Buffer
		got, err := pickAWSProfile(newPrompter(strings.NewReader(input), &out), profiles, "default")
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q", input, got, err, want)
		}
		if !strings.Contains(out.String(), "  2) work\n") || !strings.Contains(out.String(), "Enter 1-2, or another profile name [1]: ") {
			t.Fatalf("unexpected output: %s", &out)
		}
	}
}

func TestPickAWSProfileMarksProfilesWithoutCredentials(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	profiles := []AWSProfile{{Name: "default", NoCredentials: true}, {Name: "work"}}
	if _, err := pickAWSProfile(newPrompter(strings.NewReader("2\n"), &out), profiles, "work"); err != nil {
		t.Fatal(err)
	}
	want := "  1) default (no credentials configured)\n  2) work\nEnter 1-2, or another profile name [2]: "
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output %q, want %q", &out, want)
	}
}

func TestDefaultAWSProfile(t *testing.T) {
	t.Parallel()
	usable := []AWSProfile{{Name: "default"}, {Name: "work"}}
	bareDefault := []AWSProfile{{Name: "default", NoCredentials: true}, {Name: "work"}}
	for _, tc := range []struct {
		name       string
		saved      string
		awsProfile string
		profiles   []AWSProfile
		want       string
	}{
		{"saved profile wins", "saved", "work", usable, "saved"},
		{"AWS_PROFILE", "", "work", usable, "work"},
		{"AWS_PROFILE not discovered", "", "elsewhere", usable, "elsewhere"},
		{"AWS_PROFILE without credentials", "", "default", bareDefault, "default"},
		{"default profile", "", "", usable, "default"},
		{"default profile without credentials", "", "", bareDefault, "work"},
		{"only profile", "", "", []AWSProfile{{Name: "work"}}, "work"},
		{"only profile without credentials", "", "", []AWSProfile{{Name: "work", NoCredentials: true}}, ""},
		{"several profiles", "", "", []AWSProfile{{Name: "a"}, {Name: "b"}}, ""},
	} {
		env := Env{LookupEnv: func(key string) (string, bool) {
			if key == "AWS_PROFILE" && tc.awsProfile != "" {
				return tc.awsProfile, true
			}
			return "", false
		}}
		if got := defaultAWSProfile(tc.saved, tc.profiles, env); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPromptAWSProfileDefaultsToAWSProfileVariable(t *testing.T) {
	t.Parallel()
	var cfg credentials.Config
	env := Env{
		AWSProfiles: func() ([]AWSProfile, error) {
			return []AWSProfile{{Name: "default", Region: "us-east-1"}, {Name: "work", Region: "eu-west-1"}}, nil
		},
		LookupEnv: func(key string) (string, bool) {
			return map[string]string{"AWS_PROFILE": "work"}[key], key == "AWS_PROFILE"
		},
	}
	var out bytes.Buffer
	err := promptAWSProfile(newPrompter(strings.NewReader("\n"), &out), &cfg, env)
	if err != nil || cfg.AWSProfile != "work" || cfg.Region != "eu-west-1" {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

func TestStorageProviderDefaultFollowsAWSProfiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		existing string
		profiles []AWSProfile
		err      error
		want     string
	}{
		{"usable profile", "", []AWSProfile{{Name: "default", NoCredentials: true}, {Name: "work"}}, nil, "[2]: "},
		{"no usable profile", "", []AWSProfile{{Name: "default", NoCredentials: true}}, nil, "[1]: "},
		{"no profiles", "", nil, nil, "[1]: "},
		{"discovery failed", "", nil, errors.New("unreadable"), "[1]: "},
		{"saved provider kept", credentials.ProviderR2, []AWSProfile{{Name: "work"}}, nil, "[1]: "},
	} {
		env := Env{AWSProfiles: func() ([]AWSProfile, error) { return tc.profiles, tc.err }}
		var out bytes.Buffer
		// The reader ends after the provider question, so setup stops there.
		_, _, _, err := promptStorage(newPrompter(strings.NewReader(""), &out), credentials.Config{Provider: tc.existing}, env)
		if err == nil || !strings.Contains(out.String(), "Enter 1-3 "+tc.want) {
			t.Errorf("%s: err=%v output %q, want default %q", tc.name, err, &out, tc.want)
		}
	}
}
