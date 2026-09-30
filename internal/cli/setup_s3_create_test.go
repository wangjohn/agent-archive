package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// fakeCreator is a BucketCreator that answers from memory and records what it
// was asked, in order.
type fakeCreator struct {
	// create answers each CreateBucket in turn; the last answer repeats.
	create  []error
	block   []error
	deleted error
	privacy storage.PrivacyReport
	calls   []string
}

var errCreateDenied = &smithy.GenericAPIError{Code: "AccessDenied", Message: "synthetic"}

func (f *fakeCreator) next(answers *[]error) error {
	if len(*answers) == 0 {
		return nil
	}
	err := (*answers)[0]
	if len(*answers) > 1 {
		*answers = (*answers)[1:]
	}
	return err
}

func (f *fakeCreator) CreateBucket(_ context.Context, bucket, region string) error {
	f.calls = append(f.calls, "create "+bucket+" "+region)
	return f.next(&f.create)
}

func (f *fakeCreator) BlockPublicAccess(_ context.Context, bucket string) error {
	f.calls = append(f.calls, "block "+bucket)
	return f.next(&f.block)
}

func (f *fakeCreator) DeleteBucket(_ context.Context, bucket string) error {
	f.calls = append(f.calls, "delete "+bucket)
	return f.deleted
}

func (f *fakeCreator) InspectPrivacy(_ context.Context, bucket string) storage.PrivacyReport {
	f.calls = append(f.calls, "inspect "+bucket)
	return f.privacy
}

var verifiedPrivate = storage.PrivacyReport{State: "verified_private", Reason: "all_bucket_public_access_blocks_enabled"}

// sequentialNames makes newBucketName return agent-archive-1, -2, ... for
// the rest of the test. The test must not run in parallel.
func sequentialNames(t *testing.T) {
	t.Helper()
	old, n := newBucketName, 0
	newBucketName = func() string {
		n++
		return "agent-archive-" + string(rune('0'+n))
	}
	t.Cleanup(func() { newBucketName = old })
}

// createEnv is an Env whose one S3 profile, work, has credentials and the
// region profileRegion, that opens creator, and that lists no buckets unless
// finder says so. opened records each (profile, region) the creator was
// opened with.
func createEnv(profileRegion string, creator BucketCreator, finder fakeBuckets, opened *[]string) Env {
	return Env{
		AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "work", Region: profileRegion}}, nil },
		AWSBuckets:  finder.open,
		AWSBucketCreator: func(profile, region string) (BucketCreator, error) {
			if opened != nil {
				*opened = append(*opened, profile+" "+region)
			}
			return creator, nil
		},
	}
}

func runCreate(t *testing.T, env Env, cfg *credentials.Config, input string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := promptS3Bucket(newPrompter(strings.NewReader(input), &out), cfg, env, "", true)
	return out.String(), err
}

func TestCreateS3BucketInProfileRegionRecordsBucketAndPrintsPolicy(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var opened []string
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("eu-west-2", creator, fakeBuckets{}, &opened), &cfg, "\n\n\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	if cfg.Bucket != "agent-archive-1" || cfg.Region != "eu-west-2" || cfg.AWSProfile != "work" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if want := []string{"work eu-west-2"}; strings.Join(opened, ",") != strings.Join(want, ",") {
		t.Fatalf("creator opened for %q, want %q", opened, want)
	}
	want := []string{"create agent-archive-1 eu-west-2", "block agent-archive-1", "inspect agent-archive-1"}
	if strings.Join(creator.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
	for _, text := range []string{
		"Created bucket agent-archive-1 in eu-west-2.",
		"Checked: Block Public Access is on for all four settings.",
		`"Resource": "arn:aws:s3:::agent-archive-1/agent-archive/*"`,
		"separate IAM user or role",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestCreateS3BucketInUSEast1AndOtherRegionsUseTheChosenRegion(t *testing.T) {
	for _, tc := range []struct {
		profileRegion string
		input         string
		want          string
	}{
		{"us-east-1", "\n\n", "create agent-archive-1 us-east-1"},
		{"us-east-1", "ap-southeast-2\n\n", "create agent-archive-1 ap-southeast-2"},
		{"", "us-west-1\n\n", "create agent-archive-1 us-west-1"},
	} {
		sequentialNames(t)
		creator := &fakeCreator{privacy: verifiedPrivate}
		var cfg credentials.Config
		out, err := runCreate(t, createEnv(tc.profileRegion, creator, fakeBuckets{}, nil), &cfg, "\n"+tc.input)
		if err != nil || len(creator.calls) == 0 || creator.calls[0] != tc.want {
			t.Errorf("profile region %q, input %q: calls %q err=%v, want %q\n%s", tc.profileRegion, tc.input, creator.calls, err, tc.want, out)
		}
	}
}

func TestCreateS3BucketRetriesADefaultNameOnceThenAsksAboutTheNext(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("BucketAlreadyExists"))
	creator := &fakeCreator{create: []error{taken, taken, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	// Profile, region, default name; then the bucket names agent-archive-1
	// (taken, retried once as agent-archive-2, taken), so setup asks and
	// the answer, my-archive-store, is created.
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n\nmy-archive-store\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	want := []string{"create agent-archive-1 us-east-1", "create agent-archive-2 us-east-1", "create my-archive-store us-east-1", "block my-archive-store", "inspect my-archive-store"}
	if strings.Join(creator.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %q, want %q\n%s", creator.calls, want, out)
	}
	if cfg.Bucket != "my-archive-store" {
		t.Fatalf("bucket %q", cfg.Bucket)
	}
	if !strings.Contains(out, "The name agent-archive-1 is taken (bucket names are shared by everyone on AWS); trying agent-archive-2.") {
		t.Errorf("output does not say the default was replaced:\n%s", out)
	}
}

func TestCreateS3BucketAsksAgainWhenATypedNameIsTaken(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("BucketAlreadyExists"))
	creator := &fakeCreator{create: []error{taken, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nmine-alex\nmine-alex-2\n")
	if err != nil || cfg.Bucket != "mine-alex-2" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if got := creator.calls[:2]; got[0] != "create mine-alex us-east-1" || got[1] != "create mine-alex-2 us-east-1" {
		t.Fatalf("a typed name must not be replaced on its own: %q", creator.calls)
	}
}

func TestCreateS3BucketRefusesNamesS3WouldRefuseBeforeAsking(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nMy_Bucket\nab\nok-bucket\n")
	if err != nil || cfg.Bucket != "ok-bucket" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if creator.calls[0] != "create ok-bucket us-east-1" {
		t.Fatalf("an invalid name reached S3: %q", creator.calls)
	}
	for _, text := range []string{`"My_Bucket" can't be a bucket name`, `"ab" can't be a bucket name: it must be 3 to 63 characters long`} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestCreateS3BucketWithoutPermissionExplainsAndPicksAnExistingBucket(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errCreateDenied}}
	finder := fakeBuckets{names: []string{"photos", "team-archive"}, regions: map[string]string{"team-archive": "us-west-2"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, finder, nil), &cfg, "\n\n\n2\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	if cfg.Bucket != "team-archive" || cfg.Region != "us-west-2" {
		t.Fatalf("cfg=%+v, want the existing bucket picked after the fallback", cfg)
	}
	for _, text := range []string{"isn't allowed to create buckets", "s3:CreateBucket", "s3:PutBucketPublicAccessBlock", "pick an existing bucket"} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
	if strings.Join(creator.calls, ",") != "create agent-archive-1 us-east-1" {
		t.Fatalf("calls %q: nothing may follow a refused CreateBucket", creator.calls)
	}
	if strings.Contains(out, "arn:aws:s3") {
		t.Error("a policy was printed for a bucket that was not created")
	}
}

func TestCreateS3BucketProfileWithoutCredentialsFallsBackWithoutCallingS3(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{}
	env := createEnv("us-east-1", creator, fakeBuckets{}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "bare", NoCredentials: true}}, nil }
	var cfg credentials.Config
	out, err := runCreate(t, env, &cfg, "1\ntyped\nus-east-1\n")
	if err != nil || cfg.Bucket != "typed" || len(creator.calls) != 0 {
		t.Fatalf("cfg=%+v calls=%q err=%v\n%s", cfg, creator.calls, err, out)
	}
	if !strings.Contains(out, "has no credentials configured, so it can't create a bucket") {
		t.Errorf("output does not explain:\n%s", out)
	}
}

func TestCreateS3BucketOtherFailureIsExplainedWithoutQuotingS3(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{&smithy.GenericAPIError{Code: "TooManyBuckets", Message: "secret-account-detail"}}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}, nil), &cfg, "\n\n\n1\n")
	if err != nil || cfg.Bucket != "x" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "Couldn't create the bucket.") || strings.Contains(out, "secret-account-detail") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketRefusedRegionIsNotReportedAsAnotherRegionsBucket(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{&smithy.GenericAPIError{Code: "IllegalLocationConstraintException", Message: "synthetic"}}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}, nil), &cfg, "\n\n\n1\n")
	if err != nil || cfg.Bucket != "x" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "S3 didn't accept that region for a new bucket") || strings.Contains(out, "different AWS region") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketBlockPublicAccessFailureNeverContinues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    string
		wantErr   string
		wantCalls string
		wantCfg   string
	}{
		{"stop", "stop\n", "bucket agent-archive-1 was created but Block Public Access is not on", "create agent-archive-1 us-east-1,block agent-archive-1", ""},
		{"delete then an existing bucket", "delete\n1\n", "", "create agent-archive-1 us-east-1,block agent-archive-1,delete agent-archive-1", "existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequentialNames(t)
			creator := &fakeCreator{block: []error{errCreateDenied}}
			finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
			var cfg credentials.Config
			out, err := runCreate(t, createEnv("us-east-1", creator, finder, nil), &cfg, "\n\n\n"+tc.answer)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err=%v\n%s", err, out)
			}
			if strings.Join(creator.calls, ",") != tc.wantCalls {
				t.Fatalf("calls %q, want %q", creator.calls, tc.wantCalls)
			}
			if cfg.Bucket == "agent-archive-1" || cfg.Bucket != tc.wantCfg {
				t.Fatalf("bucket %q: the half-secured bucket must not be chosen", cfg.Bucket)
			}
			for _, text := range []string{"Bucket agent-archive-1 was created, but setup couldn't turn on Block Public Access", "s3:PutBucketPublicAccessBlock", "Setup won't store sessions in it"} {
				if !strings.Contains(out, text) {
					t.Errorf("output lacks %q:\n%s", text, out)
				}
			}
			if strings.Contains(out, "arn:aws:s3") {
				t.Error("a policy was printed for a bucket that was not secured")
			}
		})
	}
}

func TestCreateS3BucketBlockPublicAccessRetrySucceeds(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n\nretry\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	want := "create agent-archive-1 us-east-1,block agent-archive-1,block agent-archive-1,inspect agent-archive-1"
	if strings.Join(creator.calls, ",") != want {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
}

func TestCreateS3BucketDeleteFailureNamesTheCommand(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied}, deleted: errCreateDenied}
	finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, finder, nil), &cfg, "\n\n\ndelete\n1\n")
	if err != nil || cfg.Bucket != "existing" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "aws s3api delete-bucket --bucket agent-archive-1 --profile work") {
		t.Errorf("output lacks the manual delete command:\n%s", out)
	}
}

func TestCreateS3BucketThatStillLooksPublicIsNotUsed(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: storage.PrivacyReport{State: "public_or_risky", Reason: "public_bucket_policy"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n\nstop\n")
	if err == nil || cfg.Bucket != "" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "still looks public") {
		t.Errorf("output does not say so:\n%s", out)
	}
}

func TestCreateS3BucketUnreadableBlockPublicAccessWarnsAndContinues(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: storage.UnknownPrivacy("s3")}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv("us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "can't read it back to confirm") || strings.Contains(out, "Checked: Block Public Access is on") {
		t.Errorf("output:\n%s", out)
	}
}

func TestStorageMenuOffersS3CreationAndUsesTheS3Provider(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var out bytes.Buffer
	cfg, _, saveSecret, err := promptStorage(newPrompter(strings.NewReader("s3-new\n\n\n\n"), &out), credentials.Config{}, createEnv("us-east-1", creator, fakeBuckets{}, nil), "")
	if err != nil || saveSecret {
		t.Fatalf("err=%v saveSecret=%v\n%s", err, saveSecret, &out)
	}
	if cfg.Provider != credentials.ProviderS3 || cfg.Bucket != "agent-archive-1" || cfg.Region != "us-east-1" || cfg.AWSProfile != "work" || cfg.Prefix != defaultPrefix {
		t.Fatalf("cfg=%+v", cfg)
	}
	if !strings.Contains(out.String(), "Amazon S3: create a new private bucket for me") {
		t.Errorf("menu does not offer it:\n%s", &out)
	}
}

func TestEmptyBucketListOffersToCreateOneWithNew(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var out bytes.Buffer
	var cfg credentials.Config
	err := promptS3Location(newPrompter(strings.NewReader("\nnew\n\n\n"), &out), &cfg, createEnv("eu-west-1", creator, fakeBuckets{}, nil), "")
	if err != nil || cfg.Bucket != "agent-archive-1" || cfg.Region != "eu-west-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, &out)
	}
	if !strings.Contains(out.String(), "Type the bucket name, or new to create a private one.") {
		t.Errorf("the offer isn't shown:\n%s", &out)
	}
}

func TestEmptyBucketListCreationFailureAsksForTheNameAgain(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errCreateDenied}}
	var out bytes.Buffer
	var cfg credentials.Config
	err := promptS3Location(newPrompter(strings.NewReader("\nnew\n\n\nmine-alex\n"), &out), &cfg, createEnv("eu-west-1", creator, fakeBuckets{regions: map[string]string{"mine-alex": "us-east-1"}}, nil), "")
	if err != nil || cfg.Bucket != "mine-alex" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, &out)
	}
}

func TestBucketNameProblem(t *testing.T) {
	t.Parallel()
	for name, wantProblem := range map[string]bool{
		"agent-archive-3f9a1c2e": false,
		"abc":                    false,
		"my.bucket-1":            false,
		strings.Repeat("a", 63):  false,
		"ab":                     true,
		strings.Repeat("a", 64):  true,
		"Upper":                  true,
		"under_score":            true,
		"-leading":               true,
		"trailing-":              true,
		"has space":              true,
		"a..b":                   true,
		".dot":                   true,
		"192.168.5.4":            true,
		"xn--bucket":             true,
		"sthree-bucket":          true,
		"amzn-s3-demo-bucket":    true,
		"bucket-s3alias":         true,
		"bucket--ol-s3":          true,
		"bucket.mrap":            true,
		"bucket--x-s3":           true,
		"bucket--table-s3":       true,
	} {
		if got := bucketNameProblem(name) != ""; got != wantProblem {
			t.Errorf("bucketNameProblem(%q) = %q, want a problem: %v", name, bucketNameProblem(name), wantProblem)
		}
	}
}

func TestDefaultBucketNameIsValidAndDiffersEachTime(t *testing.T) {
	t.Parallel()
	a, b := newBucketName(), newBucketName()
	if a == b || !strings.HasPrefix(a, "agent-archive-") || bucketNameProblem(a) != "" || len(a) != len("agent-archive-")+8 {
		t.Fatalf("names %q and %q", a, b)
	}
}

// documentedRuntimePolicy is the first JSON block under "Amazon S3" in
// docs/security/bucket-permissions.md.
func documentedRuntimePolicy(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/security/bucket-permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	_, afterHeading, found := strings.Cut(doc, "\n## Amazon S3\n")
	if !found {
		t.Fatal("bucket-permissions.md has no Amazon S3 section")
	}
	_, afterFence, found := strings.Cut(afterHeading, "```json\n")
	if !found {
		t.Fatal("the Amazon S3 section has no json block")
	}
	block, _, found := strings.Cut(afterFence, "\n```")
	if !found {
		t.Fatal("the json block is not closed")
	}
	return block
}

func TestRuntimePolicyMatchesDocument(t *testing.T) {
	t.Parallel()
	if got := documentedRuntimePolicy(t); got != runtimePolicyTemplate {
		t.Fatalf("the policy setup prints has drifted from docs/security/bucket-permissions.md.\ndocument:\n%s\n\nsetup:\n%s", got, runtimePolicyTemplate)
	}
}

func TestRuntimePolicyNamesTheBucketAndPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bucket      string
		prefix      string
		wantObjects string
		wantList    string
	}{
		{"agent-archive-3f9a1c2e", "agent-archive/", "arn:aws:s3:::agent-archive-3f9a1c2e/agent-archive/*", `["agent-archive/*"]`},
		{"work-logs", "team/notes", "arn:aws:s3:::work-logs/team/notes/*", `["team/notes/*"]`},
	} {
		policy := runtimePolicy(tc.bucket, tc.prefix)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(policy), &decoded); err != nil {
			t.Fatalf("policy is not JSON: %v\n%s", err, policy)
		}
		if strings.Contains(policy, "my-archive-bucket") {
			t.Errorf("the placeholder bucket is left in:\n%s", policy)
		}
		for _, want := range []string{tc.wantObjects, tc.wantList, `"Resource": "arn:aws:s3:::` + tc.bucket + `"`} {
			if !strings.Contains(policy, want) {
				t.Errorf("policy for %s/%s lacks %q:\n%s", tc.bucket, tc.prefix, want, policy)
			}
		}
		// Creating a bucket is exactly what the runtime policy must not allow.
		for _, forbidden := range []string{"s3:CreateBucket", "s3:PutBucketPublicAccessBlock", "s3:DeleteBucket"} {
			if strings.Contains(policy, forbidden) {
				t.Errorf("the runtime policy grants %s", forbidden)
			}
		}
	}
}

// Through the whole of setup: a bucket created for the person passes the
// same storage check as any other, and the review shows it as the storage.
func TestSetupThroughStorageCheckWithACreatedS3Bucket(t *testing.T) {
	sequentialNames(t)
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	f.env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "default", Region: "us-west-2"}}, nil }
	f.env.AWSBucketCreator = func(profile, region string) (BucketCreator, error) {
		if profile != "default" || region != "us-west-2" {
			t.Errorf("creator opened for %q in %q", profile, region)
		}
		return creator, nil
	}
	// The storage question's third choice creates the bucket; the profile,
	// region and name questions take their defaults; then the review is
	// cancelled.
	out := f.runSetup(t, strings.Join([]string{"", "3", "", "", "", "", "3"}, "\n")+"\n")
	for _, text := range []string{
		"Created bucket agent-archive-1 in us-west-2.",
		"Checked: Block Public Access is on for all four settings.",
		"✓ Connected to your storage.",
		"s3://agent-archive-1/agent-archive/  us-west-2 · profile default",
		`"Resource": "arn:aws:s3:::agent-archive-1/agent-archive/*"`,
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}
