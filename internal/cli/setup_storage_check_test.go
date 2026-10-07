package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// putErrorStore fails every upload with err while err is not nil.
type putErrorStore struct {
	storage.ObjectStore
	err *error
}

func (s putErrorStore) Put(ctx context.Context, key string, value []byte) error {
	if *s.err != nil {
		return *s.err
	}
	return s.ObjectStore.Put(ctx, key, value)
}

// failingStorageEnv is a setup environment whose bucket fails uploads with
// *failure while it is set.
func failingStorageEnv(t *testing.T, home string, failure *error) Env {
	t.Helper()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return putErrorStore{bucket, failure}, nil
	}
	return env
}

const storageQuestion = "Where should your archive live?"

// After a failed check, "Continue where you left off" asks the storage
// questions again, not the check that just failed.
//
// Regression: only a failure to build a client reset the draft's step, so
// continuing after a refusal repeated the same failing check without asking
// anything.
func TestSetupContinueAfterAccessFailureAsksForStorage(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	var failure error = &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
	env := failingStorageEnv(t, home, &failure)
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	output := setupRun(t, env, input+"cancel\n", 1)
	if !setupContainsText(output, "Access denied.") {
		t.Fatalf("no diagnosis:\n%s", output)
	}

	failure = nil
	// Continue, then keep every storage answer, and start archiving.
	output = setupRun(t, env, "continue\n\n\n\ny\n", 0)
	if !setupContainsText(output, storageQuestion) {
		t.Fatalf("continuing did not ask for storage:\n%s", output)
	}
	if cfg, found, err := config.Load(home); err != nil || !found || cfg.Storage.Bucket != "bucket" {
		t.Fatalf("saved=%v err=%v cfg=%+v", found, err, cfg.Storage)
	}
}

// The failure menu's default is the fix: asking the storage questions
// again. Retry repeats the check without asking.
//
// Regression: the default was Cancel.
func TestSetupStorageFailureMenu(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer    string
		questions int
	}{
		{"", 2},
		{"retry", 1},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			t.Parallel()
			var failure error = &smithy.GenericAPIError{Code: "AccessDenied"}
			env := failingStorageEnv(t, t.TempDir(), &failure)
			input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
			// The answer, then (had it asked) the storage answers kept, and
			// the second failure stops.
			answers := tc.answer + "\n"
			if tc.questions == 2 {
				answers += "\n\n\n"
			}
			output := setupRun(t, env, input+answers+"cancel\n", 1)
			if n := strings.Count(output, storageQuestion); n != tc.questions {
				t.Fatalf("storage asked %d times, want %d:\n%s", n, tc.questions, output)
			}
			if n := strings.Count(output, "Access denied."); n != 2 {
				t.Fatalf("diagnosis printed %d times, want once per check:\n%s", n, output)
			}
			if !setupContainsText(output, "1) Change storage settings (default)") || !setupContainsText(output, "Choose [1]") {
				t.Fatalf("menu does not default to the fix:\n%s", output)
			}
		})
	}
}

// The storage error itself is printed only with --verbose, and then once:
// setup's last line only says the check failed.
//
// Regression: the error, up to about 900 characters of SDK text, was
// printed under the check and again as setup exited.
func TestSetupStorageFailurePrintsTheErrorOnlyWhenVerbose(t *testing.T) {
	t.Parallel()
	const raw = "operation error S3: PutObject, https response error StatusCode: 403"
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "verbose"}[verbose], func(t *testing.T) {
			t.Parallel()
			var failure error = &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: raw}}
			env := failingStorageEnv(t, t.TempDir(), &failure)
			args := []string{"setup"}
			if verbose {
				args = append(args, "--verbose")
			}
			input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
			var out strings.Builder
			if code := Run(args, strings.NewReader(input+"cancel\n"), &out, &out, env); code != 1 {
				t.Fatalf("exit %d\n%s", code, &out)
			}
			output := out.String()
			want := 0
			if verbose {
				want = 1
			}
			if n := strings.Count(output, raw); n != want {
				t.Fatalf("storage error printed %d times, want %d:\n%s", n, want, output)
			}
			if !setupContainsText(output, "Setup incomplete: the storage check failed; your answers are kept\n") {
				t.Fatalf("last line:\n%s", output)
			}
		})
	}
}

// For a bucket in another region, the fix asks for the region alone, with
// the bucket's own region as the default.
func TestSetupWrongRegionFixAsksForTheRegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		var failure error
		if cfg.Storage.Region != "eu-west-1" {
			failure = &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}}}},
				Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
			}
		}
		return putErrorStore{bucket, &failure}, nil
	}
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	output := setupRun(t, env, input+"\n\ny\n", 0)
	if !setupContainsText(output, "1) Change the region") || !setupContainsText(output, "? Bucket region") {
		t.Fatalf("the fix did not ask for the region:\n%s", output)
	}
	if strings.Count(output, storageQuestion) != 1 {
		t.Fatalf("the fix asked every storage question again:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); cfg.Storage.Region != "eu-west-1" {
		t.Fatalf("region %q", cfg.Storage.Region)
	}
}

// R2 has one region, "auto", so a failure the provider blames on the region
// (AuthorizationHeaderMalformed) gets no region hint there: the account in
// the endpoint is what to check.
func TestStorageDiagnosisGivesR2NoRegionHint(t *testing.T) {
	t.Parallel()
	err := &smithy.GenericAPIError{Code: "AuthorizationHeaderMalformed"}
	r2 := credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}
	d := storageDiagnosis(r2, err)
	if d.Cause != storage.CauseWrongRegion {
		t.Fatalf("cause %q", d.Cause)
	}
	for _, text := range []string{d.Explanation, d.Fix, storageFailureHeadline(r2, d), storageFixLabel(r2, d)} {
		if setupContainsText(strings.ToLower(text), "region") {
			t.Errorf("R2 diagnosis mentions a region: %q", text)
		}
	}
	if !setupContainsText(d.Fix, "account ID") {
		t.Errorf("fix %q does not point at the account", d.Fix)
	}
	s3 := credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}
	if got := storageDiagnosis(s3, err); !setupContainsText(got.Fix, "region") {
		t.Errorf("S3 fix %q lost its region hint", got.Fix)
	}
}

// wrongRegionEnv is a setup environment whose S3 bucket is in eu-west-1: the
// check fails, naming that region, for any other.
func wrongRegionEnv(t *testing.T, home string) Env {
	t.Helper()
	return wrongRegionEnvNaming(t, home, http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}})
}

// wrongRegionEnvNaming is wrongRegionEnv whose failures carry header, which
// names the bucket's region or not.
func wrongRegionEnvNaming(t *testing.T, home string, header http.Header) Env {
	t.Helper()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		var failure error
		if cfg.Storage.Region != "eu-west-1" {
			failure = &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: header}},
				Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
			}
		}
		return putErrorStore{bucket, &failure}, nil
	}
	return env
}

// Stopping at a wrong-region failure that named the bucket's region saves
// that region, so continuing checks it, without asking, rather than
// repeating the check that failed.
func TestSetupContinueAfterWrongRegionUsesTheBucketsRegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := wrongRegionEnv(t, home)
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	setupRun(t, env, input+"cancel\n", 1)
	draft, found, problem, err := readDraft(home)
	if err != nil || !found || problem != "" || draft.Step != 1 || draft.Config.Storage.Region != "eu-west-1" {
		t.Fatalf("draft step %d region %q (found=%v problem=%q err=%v), want step 1 in eu-west-1", draft.Step, draft.Config.Storage.Region, found, problem, err)
	}

	// Continue, keep every storage answer, and start archiving.
	output := setupRun(t, env, "continue\n\n\n\ny\n", 0)
	if setupContainsText(output, "The bucket is in another region.") || !setupContainsText(output, "Using region eu-west-1.") || setupContainsText(output, "Bucket region [") {
		t.Fatalf("continuing repeated the failed check:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); cfg.Storage.Region != "eu-west-1" {
		t.Fatalf("region %q", cfg.Storage.Region)
	}
}

// The region asked after a wrong-region failure, by the fix and by
// Continue, turns away an answer not shaped like a region, as the storage
// questions' own region question does.
//
// Regression: both asked with p.required, so a path typed there was saved
// as the region.
func TestSetupWrongRegionAnswerMustBeARegion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// cancel stops at the failure menu, and then is the second run's
		// input; otherwise then follows the menu's default, the fix.
		cancel bool
		then   string
	}{
		{name: "fix", then: "\n~/code/api\neu-west-1\ny\n"},
		{name: "continue", cancel: true, then: "continue\n\n\n\n~/code/api\neu-west-1\ny\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			// Continue asks the region only when S3 didn't name it.
			env := wrongRegionEnvNaming(t, home, http.Header{})
			input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
			var output string
			if tc.cancel {
				setupRun(t, env, input+"cancel\n", 1)
				output = setupRun(t, env, tc.then, 0)
			} else {
				output = setupRun(t, env, input+tc.then, 0)
			}
			if !setupContainsText(output, `"~/code/api" isn't an AWS region.`) {
				t.Fatalf("the path was not turned away:\n%s", output)
			}
			if cfg, _, _ := config.Load(home); cfg.Storage.Region != "eu-west-1" {
				t.Fatalf("region %q", cfg.Storage.Region)
			}
		})
	}
}

// secondRegionLookup is a BucketFinder whose buckets can't be listed and
// whose first region lookup is refused; later ones find region.
type secondRegionLookup struct {
	region string
	calls  *int
}

func (f secondRegionLookup) Buckets(context.Context) ([]string, error) { return nil, errAccessDenied }

func (f secondRegionLookup) Region(context.Context, string) (string, error) {
	*f.calls++
	if *f.calls == 1 {
		return "", errAccessDenied
	}
	return f.region, nil
}

// When S3 refuses the check for the region without naming one, the fix
// asks S3 for the bucket's region, as the storage questions do, and offers
// it, rather than the region that just failed.
func TestSetupWrongRegionFixLooksUpTheRegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		var failure error
		if cfg.Storage.Region != "eu-west-1" {
			failure = &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{}}},
				Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
			}
		}
		return putErrorStore{bucket, &failure}, nil
	}
	calls := 0
	env.AWSBuckets = func(string, string) (BucketFinder, error) {
		return secondRegionLookup{region: "eu-west-1", calls: &calls}, nil
	}
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	output := setupRun(t, env, input+"\n\ny\n", 0)
	if !setupContainsText(output, "Bucket bucket is in eu-west-1.") {
		t.Fatalf("the fix did not offer the looked-up region:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); cfg.Storage.Region != "eu-west-1" {
		t.Fatalf("region %q", cfg.Storage.Region)
	}
}

// After an S3 wrong-region failure, storage changed to R2 on Continue asks
// no AWS region: R2 keeps its own.
//
// Regression: the region the failure set to ask was asked whatever the
// provider, and promptRegion refused a blank answer and R2's "auto".
func TestSetupWrongRegionThenR2AsksNoRegion(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		var failure error
		if cfg.Storage.Provider == credentials.ProviderS3 {
			failure = &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}}}},
				Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
			}
		}
		return putErrorStore{bucket, &failure}, nil
	}
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	setupRun(t, env, input+"cancel\n", 1)
	output := setupRun(t, env, "continue\nr2-existing\n0123456789abcdef0123456789abcdef\ntest-bucket\nACCESS\nsecret\ny\n", 0)
	if setupContainsText(output, "Bucket region") {
		t.Fatalf("R2 was asked an AWS region:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); cfg.Storage.Provider != credentials.ProviderR2 {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// For refused R2 credentials, the fix ("Enter the R2 access key again")
// asks for the key, rather than offering to keep the one just refused.
func TestSetupR2CredentialFixAsksForTheKey(t *testing.T) {
	t.Parallel()
	var failure error = &smithy.GenericAPIError{Code: "InvalidAccessKeyId"}
	env := failingStorageEnv(t, t.TempDir(), &failure)
	input := strings.TrimSuffix(r2SetupInput(t.TempDir(), "first-secret"), "y\n")
	// The fix, the kept provider, account and bucket, a new key, and the
	// second failure stops.
	output := setupRun(t, env, input+"\n\n\n\nACCESS2\nsecond-secret\ncancel\n", 1)
	if !setupContainsText(output, "1) Enter the R2 access key again") {
		t.Fatalf("fix label:\n%s", output)
	}
	if setupContainsText(output, "Keep stored R2 credentials?") || strings.Count(output, "? Access key ID") != 2 {
		t.Fatalf("the fix did not ask for the key:\n%s", output)
	}
}

// Even with --verbose, a credential_process failure's own text is withheld:
// the SDK's message quotes what the program printed, which can be
// credentials.
func TestSetupVerboseWithholdsCredentialProcessOutput(t *testing.T) {
	t.Parallel()
	const secret = "printed-secret-access-key"
	var failure error = &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &processcreds.ProviderError{Err: errors.New("parse failed of process output: {\"SecretAccessKey\":\"" + secret + "\"}")}}
	env := failingStorageEnv(t, t.TempDir(), &failure)
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	var out strings.Builder
	if code := Run([]string{"setup", "--verbose"}, strings.NewReader(input+"cancel\n"), &out, &out, env); code != 1 {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	if setupContainsText(out.String(), secret) || !setupContainsText(out.String(), "Details: not shown") {
		t.Fatalf("credential_process output printed:\n%s", &out)
	}
}

// setup --yes prints the diagnosis on standard error, with its last line, so
// a script that keeps only errors still learns the cause.
func TestSetupYesStorageDiagnosisGoesToStandardError(t *testing.T) {
	t.Parallel()
	var failure error = &smithy.GenericAPIError{Code: "AccessDenied"}
	env := failingStorageEnv(t, t.TempDir(), &failure)
	env.IsTerminal = func(any) bool { return false }
	var out, errOut strings.Builder
	args := []string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "work", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects"}
	if code := Run(args, strings.NewReader(""), &out, &errOut, env); code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, &out, &errOut)
	}
	if !setupContainsText(errOut.String(), "Access denied.") || setupContainsText(out.String(), "refused") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", &out, &errOut)
	}
}

// The setup check's read-back failing is neither access nor the network:
// each sentinel gets its own plain diagnosis, naming the test file.
//
// Regression: both fell to "agent-archive couldn't use the storage
// settings", which blamed settings that had just worked for the upload.
func TestStorageDiagnosisNamesTheReadBack(t *testing.T) {
	t.Parallel()
	s3 := credentials.Config{Provider: credentials.ProviderS3, Bucket: "b", AWSProfile: "work"}
	for name, err := range map[string]error{
		"changed": fmt.Errorf("setup test read: %w", storage.ErrChecksumMismatch),
		"missing": fmt.Errorf("setup test read: %w", storage.ErrNotFound),
	} {
		d := storageDiagnosis(s3, err)
		if d.Cause != storage.CauseOther || !setupContainsText(d.Explanation, "test file") || !setupContainsText(d.Explanation, "read back") {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// storage.Diagnose itself says nothing of a test file: its other
	// callers' reads share these sentinels.
	if d := storage.Diagnose(storage.ErrNotFound); setupContainsText(d.Explanation, "test file") {
		t.Errorf("Diagnose(ErrNotFound) = %+v", d)
	}
}

// After a failed check and Stop, the next setup's "Continue where you left
// off" asks the storage questions again before any check, whatever the
// cause, and asks again for the answer the cause blames when the storage
// questions would otherwise keep it: the region, and the R2 key.
//
// Regression: after a wrong-region failure that named no region, or an R2
// key the provider refused, Continue kept the failed answer without asking
// and repeated the same check.
func TestSetupContinueAfterEachFailureAsksBeforeChecking(t *testing.T) {
	t.Parallel()
	redirect := func(header http.Header) error {
		return &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: header}},
			Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
		}
	}
	for _, tc := range []struct {
		name    string
		r2      bool
		failure error
		// open, when set, replaces the failing bucket.
		open func(storage.ObjectStore) (storage.ObjectStore, error)
		// ask is what Continue must ask, or for a region S3 named, say it
		// uses, before checking.
		ask string
		// lookupAsk, when set, is that question when S3 answers the
		// bucket region lookup with eu-west-2.
		lookupAsk string
	}{
		{name: "no credentials", failure: &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.OperationError{ServiceID: "ec2imds", OperationName: "GetMetadata", Err: errors.New("host is down")}}, ask: "? AWS profile"},
		{name: "R2 key refused", r2: true, failure: &smithy.GenericAPIError{Code: "InvalidAccessKeyId"}, ask: "Access key ID"},
		{name: "access denied", failure: &smithy.GenericAPIError{Code: "AccessDenied"}, ask: storageQuestion},
		{name: "no such bucket", failure: &smithy.GenericAPIError{Code: "NoSuchBucket"}, ask: "? Bucket name"},
		{name: "wrong region named", failure: redirect(http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}}), ask: "Using region eu-west-1.", lookupAsk: "? Bucket region"},
		{name: "wrong region unnamed", failure: redirect(http.Header{}), ask: "The last storage check failed with region us-east-1.", lookupAsk: "The last storage check failed with region eu-west-2."},
		{name: "network", failure: &smithyhttp.RequestSendError{Err: errors.New("no such host")}, ask: storageQuestion},
		{name: "other", failure: errors.New("incorrect region or folder"), ask: storageQuestion},
		{name: "read-back", open: func(bucket storage.ObjectStore) (storage.ObjectStore, error) {
			return changedReadStore{bucket}, nil
		}, ask: storageQuestion},
		{name: "connect", open: func(storage.ObjectStore) (storage.ObjectStore, error) {
			return nil, errors.New("offline")
		}, ask: storageQuestion},
	} {
		// After a region lookup, the storage questions settle the region
		// without asking it, so the failed answer must be asked even so.
		for _, lookup := range []bool{false, true} {
			if lookup && tc.r2 {
				continue
			}
			name := tc.name
			if lookup {
				name += " after region lookup"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				failure := tc.failure
				env := failingStorageEnv(t, home, &failure)
				if tc.open != nil {
					bucket := storagetest.NewMemoryStore()
					env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return tc.open(bucket) }
				}
				input := s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir())
				if tc.r2 {
					input = r2SetupInput(t.TempDir(), "first-secret")
				}
				ask := tc.ask
				if lookup {
					// The bucket is typed, and S3 names its region.
					env.AWSBuckets = fakeBuckets{listErr: errAccessDenied, regions: map[string]string{"bucket": "eu-west-2"}}.open
					input = strings.Replace(input, "\nbucket\nus-east-1\n", "\nbucket\n", 1)
					ask = firstNonEmpty(tc.lookupAsk, tc.ask)
				}
				first := setupRun(t, env, strings.TrimSuffix(input, "y\n")+"cancel\n", 1)
				if lookup && !setupContainsText(first, "Bucket bucket is in eu-west-2; using that region.") {
					t.Fatalf("setup did not use the looked-up region:\n%s", first)
				}

				// A real second run on the saved draft: Continue, then keep
				// every default until the answers run out.
				output := setupRun(t, env, "continue\n"+strings.Repeat("\n", 8), 1)
				check := strings.Index(output, "Checking your storage connection")
				asked := strings.Index(output, ask)
				if asked < 0 || check >= 0 && check < asked {
					t.Fatalf("Continue checked before asking %q:\n%s", ask, output)
				}
				// Setup doesn't promise to keep a region it is about to ask.
				if setupContainsText(ask, "The last storage check failed") && setupContainsText(output, "You can change it at the final review.") {
					t.Fatalf("Continue said it would use the region it then asked for:\n%s", output)
				}
				if tc.r2 && setupContainsText(output, "Keep stored R2 credentials?") {
					t.Fatalf("Continue offered to keep the refused key:\n%s", output)
				}
			})
		}
	}
}

func TestSetupStorageDiagnosticDetailsDoesNotRetry(t *testing.T) {
	t.Parallel()
	failure := errors.New("synthetic storage transport failure")
	env := failingStorageEnv(t, t.TempDir(), &failure)
	open := env.OpenStore
	calls := 0
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) { calls++; return open(cfg) }
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
	out := setupRun(t, env, input+"details\ncancel\n", 1)
	if calls != 1 || strings.Count(out, "Checking your storage connection") != 1 || !setupContainsText(out, "synthetic storage transport failure") || strings.Count(out, "? What next?") != 2 {
		t.Fatalf("details retried or left recovery: opens=%d\n%s", calls, out)
	}
}
