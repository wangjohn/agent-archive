package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/wangjohn/agent-archive/internal/config"
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
	t.Parallel()
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
	_, _, _, _ = promptStorage(newPrompter(strings.NewReader(""), &out), credentials.Config{Provider: credentials.ProviderS3}, env, "")
	if called {
		t.Fatal("AWS profile discovery ran although a provider was saved")
	}
	if !strings.Contains(out.String(), storageMenuPromptS3()) {
		t.Fatalf("output %q, want the saved S3 as default", &out)
	}
}

func TestAWSProfileSwitchDoesNotReuseOldRegion(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{AWSProfile: "old", Region: "us-east-1"}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "new"}}, nil }}
	var out bytes.Buffer
	err := promptS3Location(newPrompter(strings.NewReader("new\nbucket\neu-west-1\n"), &out), &cfg, env, "")
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
		if !strings.Contains(out.String(), "  2) work\n") || !strings.Contains(out.String(), "Enter a number or profile name.") || !strings.Contains(out.String(), "1) default (default)") || !strings.Contains(out.String(), "Choose [1]:") {
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
	want := "  1) default · (no credentials configured)\n  2) work (default)"
	if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "Choose [2]:") {
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
	err := promptS3Location(newPrompter(strings.NewReader("\nbucket\n"), &out), &cfg, env, "")
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
		_, _, _, err := promptStorage(newPrompter(strings.NewReader(""), &out), credentials.Config{Provider: tc.existing}, env, "")
		if err == nil || !strings.Contains(out.String(), "Choose "+tc.want) {
			t.Errorf("%s: err=%v output %q, want default %q", tc.name, err, &out, tc.want)
		}
	}
}

// fakeBuckets is a BucketFinder that answers from memory.
type fakeBuckets struct {
	names     []string
	listErr   error
	regions   map[string]string
	regionErr error
	// opened records the profile and region each open was given.
	opened *[]string
	// regionCalls counts Region lookups.
	regionCalls *int
}

func (f fakeBuckets) open(profile, region string) (BucketFinder, error) {
	if f.opened != nil {
		*f.opened = append(*f.opened, profile+" "+region)
	}
	return f, nil
}

func (f fakeBuckets) Buckets(context.Context) ([]string, error) { return f.names, f.listErr }

func (f fakeBuckets) Region(_ context.Context, bucket string) (string, error) {
	if f.regionCalls != nil {
		*f.regionCalls++
	}
	if f.regionErr != nil {
		return "", f.regionErr
	}
	region, ok := f.regions[bucket]
	if !ok {
		return "", &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "synthetic"}
	}
	return region, nil
}

var errAccessDenied = &smithy.GenericAPIError{Code: "AccessDenied", Message: "synthetic"}

func TestValidRegion(t *testing.T) {
	t.Parallel()
	for _, region := range []string{"us-east-1", "eu-west-2", "us-gov-west-1", "ap-southeast-4", "cn-north-1", "eusc-de-east-1"} {
		if !validRegion(region) {
			t.Errorf("%q rejected", region)
		}
	}
	for _, region := range []string{"", "~/code/api", "us-east", "US-EAST-1", "us_east_1", "auto", "us-east-1 ", "https://s3.us-east-1.amazonaws.com"} {
		if validRegion(region) {
			t.Errorf("%q accepted", region)
		}
	}
}

// s3LocationEnv is an Env whose AWS profiles and buckets come from memory.
func s3LocationEnv(profiles []AWSProfile, buckets fakeBuckets) Env {
	return Env{
		AWSProfiles: func() ([]AWSProfile, error) { return profiles, nil },
		AWSBuckets:  buckets.open,
	}
}

func TestS3LocationUsesTheListedBucketAndItsOwnRegion(t *testing.T) {
	t.Parallel()
	var opened []string
	// The saved region is the old bucket's; the chosen bucket's wins.
	cfg := credentials.Config{AWSProfile: "work", Region: "us-east-1"}
	env := s3LocationEnv([]AWSProfile{{Name: "work", Region: "us-east-1"}},
		fakeBuckets{names: []string{"photos", "team-archive"}, regions: map[string]string{"team-archive": "ap-southeast-2"}, opened: &opened})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("\n2\n"), &out), &cfg, env, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "team-archive" || cfg.Region != "ap-southeast-2" || cfg.AWSProfile != "work" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if !reflect.DeepEqual(opened, []string{"work us-east-1"}) {
		t.Fatalf("finder opened with %q", opened)
	}
	for _, want := range []string{"  1) photos\n  2) team-archive", "Bucket team-archive is in ap-southeast-2; using that region.\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q, want %q", &out, want)
		}
	}
}

func TestS3LocationBucketDefault(t *testing.T) {
	t.Parallel()
	names := []string{"photos", "agent-archive-alex", "agent-archive-old"}
	regions := map[string]string{"photos": "us-east-1", "agent-archive-alex": "us-east-1", "elsewhere": "us-east-1"}
	for _, tc := range []struct {
		name   string
		saved  string
		want   string
		prompt string
	}{
		{"named like agent-archive", "", "agent-archive-alex", "Choose [2]: "},
		{"saved bucket listed", "photos", "photos", "Choose [1]: "},
		{"saved bucket not listed", "elsewhere", "elsewhere", "Choose [elsewhere]: "},
	} {
		cfg := credentials.Config{AWSProfile: "work", Bucket: tc.saved}
		env := s3LocationEnv([]AWSProfile{{Name: "work"}}, fakeBuckets{names: names, regions: regions})
		var out bytes.Buffer
		if err := promptS3Location(newPrompter(strings.NewReader("\n\n"), &out), &cfg, env, ""); err != nil || cfg.Bucket != tc.want {
			t.Errorf("%s: bucket %q err=%v, want %q", tc.name, cfg.Bucket, err, tc.want)
		}
		if !strings.Contains(out.String(), tc.prompt) {
			t.Errorf("%s: output %q, want %q", tc.name, &out, tc.prompt)
		}
	}
}

func TestS3LocationListsAtMostTwentyBuckets(t *testing.T) {
	t.Parallel()
	var names []string
	for i := 1; i <= 25; i++ {
		names = append(names, fmt.Sprintf("bucket-%02d", i))
	}
	cfg := credentials.Config{AWSProfile: "work"}
	env := s3LocationEnv([]AWSProfile{{Name: "work"}}, fakeBuckets{names: names, regions: map[string]string{"bucket-25": "us-west-2"}})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("\nbucket-25\n"), &out), &cfg, env, ""); err != nil || cfg.Bucket != "bucket-25" || cfg.Region != "us-west-2" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if !strings.Contains(out.String(), "  20) bucket-20\n  5 more available by name") || strings.Contains(out.String(), "21) ") {
		t.Fatalf("output %q", &out)
	}
}

func TestS3LocationFallsBackToTyping(t *testing.T) {
	t.Parallel()
	profiles := []AWSProfile{{Name: "work", Region: "eu-west-1"}}
	for _, tc := range []struct {
		name    string
		buckets fakeBuckets
		input   string
		region  string
		notes   []string
	}{
		{
			"listing denied, region found", fakeBuckets{listErr: errAccessDenied, regions: map[string]string{"typed": "us-west-2"}},
			"\ntyped\n", "us-west-2",
			[]string{"Couldn't list buckets for profile work (access denied). Type the bucket name.", "Bucket typed is in us-west-2; using that region."},
		},
		{
			"no buckets", fakeBuckets{regions: map[string]string{"typed": "us-west-2"}},
			"\ntyped\n", "us-west-2",
			[]string{"Profile work can't see any buckets. Type the bucket name; to create one instead, choose Amazon S3 at the storage question and continue with creation."},
		},
		{
			"region denied, profile's used", fakeBuckets{names: []string{"typed"}, regionErr: errAccessDenied},
			"\n1\n", "eu-west-1",
			[]string{"Couldn't look up the region of bucket typed (access denied).\nUsing region eu-west-1. You can change it at the final review."},
		},
		{
			"region not region-shaped", fakeBuckets{names: []string{"typed"}, regions: map[string]string{"typed": "<html>"}},
			"\n1\n", "eu-west-1",
			[]string{"Couldn't look up the region of bucket typed (S3 didn't name a region)."},
		},
		{
			"no such bucket", fakeBuckets{listErr: errAccessDenied},
			"\ntyped\n", "eu-west-1",
			[]string{"Couldn't look up the region of bucket typed (no such bucket)."},
		},
	} {
		cfg := credentials.Config{}
		var out bytes.Buffer
		err := promptS3Location(newPrompter(strings.NewReader(tc.input), &out), &cfg, s3LocationEnv(profiles, tc.buckets), "")
		if err != nil || cfg.Bucket != "typed" || cfg.Region != tc.region {
			t.Errorf("%s: cfg=%+v err=%v", tc.name, cfg, err)
		}
		for _, note := range tc.notes {
			if !strings.Contains(out.String(), note) {
				t.Errorf("%s: output %q, want %q", tc.name, &out, note)
			}
		}
	}
}

func TestS3LocationUsesTheRegionAWrongRegionRefusalNames(t *testing.T) {
	t.Parallel()
	response := &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"X-Amz-Bucket-Region": {"eu-central-1"}}}
	moved := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: response}, Err: &smithy.GenericAPIError{Code: "PermanentRedirect"}}
	cfg := credentials.Config{}
	env := s3LocationEnv([]AWSProfile{{Name: "work", Region: "us-east-1"}}, fakeBuckets{names: []string{"team-archive"}, regionErr: moved})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("\n1\n"), &out), &cfg, env, ""); err != nil || cfg.Region != "eu-central-1" {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

func TestS3LocationSkipsListingForAProfileWithoutCredentials(t *testing.T) {
	t.Parallel()
	var opened []string
	cfg := credentials.Config{}
	env := s3LocationEnv([]AWSProfile{{Name: "bare", NoCredentials: true}}, fakeBuckets{opened: &opened})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("1\nteam-archive\nus-east-2\n"), &out), &cfg, env, ""); err != nil || cfg.Bucket != "team-archive" || cfg.Region != "us-east-2" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if len(opened) != 0 || !strings.Contains(out.String(), "Profile bare has no credentials configured, so type the bucket name.\n") {
		t.Fatalf("opened=%q output=%q", opened, &out)
	}
}

func TestS3LocationRejectsATypedRegionThatIsNotARegion(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{}
	env := s3LocationEnv(nil, fakeBuckets{listErr: errAccessDenied, regionErr: errAccessDenied})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("work\nteam-archive\n~/code/api\nus-east-1\n"), &out), &cfg, env, ""); err != nil || cfg.Region != "us-east-1" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if !strings.Contains(out.String(), `"~/code/api" isn't an AWS region. Enter one like us-east-1 or eu-west-2`) {
		t.Fatalf("output %q", &out)
	}
	// A saved region that isn't one is asked for again, not reused.
	cfg = credentials.Config{AWSProfile: "work", Bucket: "team-archive", Region: "~/code/api"}
	out.Reset()
	if err := promptS3Location(newPrompter(strings.NewReader("\n\neu-west-1\n"), &out), &cfg, env, ""); err != nil || cfg.Region != "eu-west-1" {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

// Discovery errors can carry whatever a credential_process printed, so
// setup names only the cause.
func TestS3LocationNeverPrintsTheDiscoveryError(t *testing.T) {
	t.Parallel()
	const secret = "AKIASYNTHETICSECRET/wJalrSynthetic"
	leak := fmt.Errorf("credential_process printed %s", secret)
	for _, buckets := range []fakeBuckets{{listErr: leak, regionErr: leak}, {names: []string{"b"}, regionErr: leak}} {
		cfg := credentials.Config{}
		env := s3LocationEnv([]AWSProfile{{Name: "work", Region: "us-east-1"}}, buckets)
		var out bytes.Buffer
		if err := promptS3Location(newPrompter(strings.NewReader("\nb\n"), &out), &cfg, env, ""); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "AKIA") || strings.Contains(out.String(), "wJalr") {
			t.Fatalf("output quotes the error: %q", &out)
		}
	}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return nil, nil }, AWSBuckets: func(string, string) (BucketFinder, error) { return nil, leak }}
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("work\nb\nus-east-1\n"), &out), &credentials.Config{}, env, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "AKIA") || !strings.Contains(out.String(), "Couldn't list buckets for profile work (the lookup failed). Type the bucket name.\n") {
		t.Fatalf("output %q", &out)
	}
}

func TestReviewRegionEditRejectsARegionThatIsNotARegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	out := setupRun(t, env, input+"edit\nregion\n~/code/api\neu-west-1\ny\n", 0)
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.Storage.Region != "eu-west-1" {
		t.Fatalf("saved=%v err=%v storage=%+v", found, err, cfg.Storage)
	}
	if !strings.Contains(out, `"~/code/api" isn't an AWS region.`) {
		t.Fatalf("output %s", out)
	}
}

func TestSetupYesRejectsARegionThatIsNotARegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	out := setupYes(t, env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "~/code/api", "--project", t.TempDir())
	if !strings.Contains(out, `"~/code/api" isn't an AWS region`) {
		t.Fatalf("output %s", out)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("a configuration was saved")
	}
	if !strings.Contains(out, `--region "~/code/api" isn't an AWS region`) {
		t.Fatalf("output does not name the rejected flag: %s", out)
	}
	// A profile's own region is checked too, and the error names the profile.
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "p", Region: "nowhere"}}, nil }
	out = setupYes(t, env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--project", t.TempDir())
	if !strings.Contains(out, `AWS profile p names region "nowhere", which isn't an AWS region; pass --region`) {
		t.Fatalf("output %s", out)
	}
}

// A region saved before setup checked its shape never reaches the
// discovery client, where it would break listing.
func TestS3LocationNeverOpensDiscoveryWithASavedRegionThatIsNotARegion(t *testing.T) {
	t.Parallel()
	var opened []string
	cfg := credentials.Config{AWSProfile: "work", Region: "~/code/api"}
	env := s3LocationEnv([]AWSProfile{{Name: "work", Region: "eu-west-2"}},
		fakeBuckets{names: []string{"team-archive"}, regions: map[string]string{"team-archive": "eu-west-2"}, opened: &opened})
	var out bytes.Buffer
	if err := promptS3Location(newPrompter(strings.NewReader("\n1\n"), &out), &cfg, env, ""); err != nil || cfg.Region != "eu-west-2" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if !reflect.DeepEqual(opened, []string{"work eu-west-2"}) {
		t.Fatalf("finder opened with %q", opened)
	}
}

// When listing fails for want of credentials, setup does not run the
// credential chain again for the region, and says so in one line.
func TestS3LocationSkipsTheRegionLookupWhenListingCannotWork(t *testing.T) {
	t.Parallel()
	noCredentials := &smithy.GenericAPIError{Code: "ExpiredToken", Message: "synthetic"}
	for _, tc := range []struct {
		name    string
		listErr error
		calls   int
	}{
		{"no credentials", noCredentials, 0},
		{"access denied", errAccessDenied, 1},
	} {
		calls := 0
		cfg := credentials.Config{}
		env := s3LocationEnv([]AWSProfile{{Name: "work", Region: "us-west-2"}},
			fakeBuckets{listErr: tc.listErr, regions: map[string]string{"typed": "us-west-2"}, regionCalls: &calls})
		var out bytes.Buffer
		if err := promptS3Location(newPrompter(strings.NewReader("\ntyped\n"), &out), &cfg, env, ""); err != nil || cfg.Region != "us-west-2" {
			t.Fatalf("%s: cfg=%+v err=%v", tc.name, cfg, err)
		}
		if calls != tc.calls || strings.Count(out.String(), "Couldn't") != 1 {
			t.Errorf("%s: %d region lookups, want %d; output %q", tc.name, calls, tc.calls, &out)
		}
	}
}

// Every AWS profile is listed by number; only buckets are capped.
func TestPickAWSProfileListsEveryProfile(t *testing.T) {
	t.Parallel()
	var profiles []AWSProfile
	for i := 1; i <= 25; i++ {
		profiles = append(profiles, AWSProfile{Name: fmt.Sprintf("account-%02d", i)})
	}
	var out bytes.Buffer
	got, err := pickAWSProfile(newPrompter(strings.NewReader("25\n"), &out), profiles, "")
	if err != nil || got != "account-25" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if !strings.Contains(out.String(), "  25) account-25\n") || strings.Contains(out.String(), "more not listed") {
		t.Fatalf("output %q", &out)
	}
}

// Discovery uses the region the SDK finds (AWS_REGION, then the profile)
// when none is given, and falls back to us-east-1 only when there is none,
// so GovCloud or China credentials reach their own partition.
func TestBucketDiscoveryConfigKeepsTheSDKRegion(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	body := "[profile gov]\nregion = us-gov-west-1\naws_access_key_id = AKIASYNTHETIC\naws_secret_access_key = synthetic\n" +
		"[profile bare]\naws_access_key_id = AKIASYNTHETIC\naws_secret_access_key = synthetic\n"
	if err := os.WriteFile(configFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_DEFAULT_REGION", "")
	for _, tc := range []struct {
		profile   string
		region    string
		awsRegion string
		want      string
	}{
		{"gov", "", "", "us-gov-west-1"},
		{"bare", "", "cn-north-1", "cn-north-1"},
		{"bare", "", "", "us-east-1"},
		{"gov", "eu-west-2", "", "eu-west-2"},
	} {
		t.Setenv("AWS_REGION", tc.awsRegion)
		cfg, err := loadBucketDiscoveryConfig(context.Background(), tc.profile, tc.region)
		if err != nil || cfg.Region != tc.want {
			t.Errorf("%s %q AWS_REGION=%q: region %q err=%v, want %q", tc.profile, tc.region, tc.awsRegion, cfg.Region, err, tc.want)
		}
	}
}

// The provider and profile questions agree: when AWS_PROFILE names a
// profile, S3 is offered first and so is that profile, even one discovery
// found no credentials for.
func TestStorageDefaultsFollowAWSProfileVariable(t *testing.T) {
	t.Parallel()
	env := Env{
		AWSProfiles: func() ([]AWSProfile, error) {
			return []AWSProfile{{Name: "bare", NoCredentials: true}}, nil
		},
		LookupEnv: func(key string) (string, bool) {
			return map[string]string{"AWS_PROFILE": "bare"}[key], key == "AWS_PROFILE"
		},
		AWSBuckets: fakeBuckets{}.open,
	}
	var out bytes.Buffer
	_, _, _, err := promptStorage(newPrompter(strings.NewReader("\n\n"), &out), credentials.Config{}, env, "")
	if err == nil {
		t.Fatal("setup went past the bucket question")
	}
	want := "1) bare · (no credentials configured)"
	if !strings.Contains(out.String(), storageMenuPromptS3()) || !strings.Contains(out.String(), want) {
		t.Fatalf("output %q, want %q", &out, want)
	}
}
