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

const storageQuestion = "Where should sessions be stored?"

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
	if !strings.Contains(output, "Access denied.") {
		t.Fatalf("no diagnosis:\n%s", output)
	}

	failure = nil
	// Continue, then keep every storage answer, and start archiving.
	output = setupRun(t, env, "continue\n\n\n\ny\n", 0)
	if !strings.Contains(output, storageQuestion) {
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
			if !strings.Contains(output, "What next?\n  1) Change storage settings\n") || !strings.Contains(output, "Enter 1-4 [1]") {
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
			if !strings.Contains(output, "Setup incomplete: the storage check failed; your answers are kept\n") {
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
	if !strings.Contains(output, "1) Change the region") || !strings.Contains(output, "Bucket region [eu-west-1]") {
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
		if strings.Contains(strings.ToLower(text), "region") {
			t.Errorf("R2 diagnosis mentions a region: %q", text)
		}
	}
	if !strings.Contains(d.Fix, "account ID") {
		t.Errorf("fix %q does not point at the account", d.Fix)
	}
	s3 := credentials.Config{Provider: credentials.ProviderS3, Bucket: "b"}
	if got := storageDiagnosis(s3, err); !strings.Contains(got.Fix, "region") {
		t.Errorf("S3 fix %q lost its region hint", got.Fix)
	}
}

// wrongRegionEnv is a setup environment whose S3 bucket is in eu-west-1: the
// check fails, naming that region, for any other.
func wrongRegionEnv(t *testing.T, home string) Env {
	t.Helper()
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
	return env
}

// Stopping at a wrong-region failure saves the bucket's own region, so that
// continuing, which keeps a saved region without asking, checks that one
// rather than repeating the check that failed.
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
	if strings.Contains(output, "The bucket is in another region.") || !strings.Contains(output, "Using region eu-west-1.") {
		t.Fatalf("continuing repeated the failed check:\n%s", output)
	}
	if cfg, _, _ := config.Load(home); cfg.Storage.Region != "eu-west-1" {
		t.Fatalf("region %q", cfg.Storage.Region)
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
	if !strings.Contains(output, "1) Enter the R2 access key again") {
		t.Fatalf("fix label:\n%s", output)
	}
	if strings.Contains(output, "Keep stored R2 credentials?") || strings.Count(output, "Access key ID") != 2 {
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
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "Details: not shown") {
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
	args := []string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "work", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "codex"}
	if code := Run(args, strings.NewReader(""), &out, &errOut, env); code != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, &out, &errOut)
	}
	if !strings.Contains(errOut.String(), "Access denied.") || strings.Contains(out.String(), "refused") {
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
		if d.Cause != storage.CauseOther || !strings.Contains(d.Explanation, "test file") || !strings.Contains(d.Explanation, "read back") {
			t.Errorf("%s: %+v", name, d)
		}
	}
	// storage.Diagnose itself says nothing of a test file: its other
	// callers' reads share these sentinels.
	if d := storage.Diagnose(storage.ErrNotFound); strings.Contains(d.Explanation, "test file") {
		t.Errorf("Diagnose(ErrNotFound) = %+v", d)
	}
}
