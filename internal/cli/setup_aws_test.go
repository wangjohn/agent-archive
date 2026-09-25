package cli

import (
	"bytes"
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
	data := "[default]\nregion = us-east-1\n[profile work]\nregion = eu-west-1\ncredential_process = touch " + marker + "\n[sso-session company]\nsso_region = us-west-2\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsPath, []byte("[legacy]\naws_access_key_id = synthetic\naws_secret_access_key = synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	profiles, err := (Env{}).awsProfiles()
	want := []AWSProfile{{"default", "us-east-1"}, {"legacy", ""}, {"work", "eu-west-1"}}
	if err != nil || !reflect.DeepEqual(profiles, want) {
		t.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("credential process executed")
	}
}

func TestAWSProfileSwitchDoesNotReuseOldRegion(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{AWSProfile: "old", Region: "us-east-1"}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{"new", ""}}, nil }}
	var out bytes.Buffer
	err := promptAWSProfile(newPrompter(strings.NewReader("new\neu-west-1\n"), &out), &cfg, env)
	if err != nil || cfg.Region != "eu-west-1" || cfg.AWSProfile != "new" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestPickAWSProfileByNumberOrName(t *testing.T) {
	t.Parallel()
	names := []string{"default", "work"}
	for input, want := range map[string]string{"2\n": "work", "\n": "default", "other\n": "other"} {
		var out bytes.Buffer
		got, err := pickAWSProfile(newPrompter(strings.NewReader(input), &out), names, "default")
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q", input, got, err, want)
		}
		if !strings.Contains(out.String(), "  2) work\n") || !strings.Contains(out.String(), "Enter 1-2, or another profile name [1]: ") {
			t.Fatalf("unexpected output: %s", &out)
		}
	}
}
