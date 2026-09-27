package storage

import (
	"context"
	"errors"
	"net"
	"regexp"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// Cause names why a storage call failed, in terms a person can act on.
type Cause string

// The causes Diagnose tells apart.
const (
	// CauseNoCredentials means there are no working credentials: none were
	// found, the ones found have expired, or the provider does not
	// recognize the access key.
	CauseNoCredentials Cause = "no_credentials"
	// CauseAccessDenied means the provider refused access to the bucket.
	CauseAccessDenied Cause = "access_denied"
	// CauseNoSuchBucket means the bucket does not exist.
	CauseNoSuchBucket Cause = "no_such_bucket"
	// CauseWrongRegion means the bucket is in another AWS region than the
	// one configured.
	CauseWrongRegion Cause = "wrong_region"
	// CauseNetwork means the provider could not be reached, or did not
	// answer in time.
	CauseNetwork Cause = "network"
	// CauseOther is any failure Diagnose does not recognize.
	CauseOther Cause = "other"
)

// Diagnosis is a storage failure in plain words: what went wrong and what to
// do about it. Neither sentence quotes the underlying error, which can hold
// whatever a profile's credential_process printed, credentials included.
type Diagnosis struct {
	Cause Cause
	// Explanation is one sentence saying what went wrong.
	Explanation string
	// Fix is one sentence saying what to do next. Where it names a
	// command that needs the AWS profile, it writes <profile> for the
	// caller to replace with the configured name.
	Fix string
	// Region is the bucket's own region, when a wrong_region failure
	// names it, and empty otherwise.
	Region string
}

// Diagnose classifies an error from a storage call, or from building a
// store with NewConfiguredStore. It trusts only typed evidence (the SDK's
// error types and API error codes, which for a HEAD request are the HTTP
// status) and never matches error text, so a message that merely mentions "denied" or "region" is not taken
// for that cause. Diagnose(nil) is the zero Diagnosis.
func Diagnose(err error) Diagnosis {
	if err == nil {
		return Diagnosis{}
	}
	// Credentials come first: fetching them can itself fail with an API
	// error or a network error (an unreachable EC2 metadata service), and
	// that is still a missing credential, not a bucket or network problem.
	if d, ok := diagnoseCredentials(err); ok {
		return d
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if d, ok := diagnoseCode(err, apiErr.ErrorCode()); ok {
			return d
		}
	}
	if isNetworkError(err) {
		return Diagnosis{
			Cause:       CauseNetwork,
			Explanation: "agent-archive couldn't reach the storage provider.",
			Fix:         "Check your internet connection, and any VPN or proxy, then try again.",
		}
	}
	return Diagnosis{
		Cause:       CauseOther,
		Explanation: "The storage provider returned an error agent-archive doesn't recognize.",
		Fix:         "Try again in a moment; if it keeps failing, check the bucket's settings with your storage provider.",
	}
}

var (
	noCredentialsFound = Diagnosis{
		Cause:       CauseNoCredentials,
		Explanation: "No credentials were found for the storage profile.",
		Fix:         "Add them with `aws configure --profile <profile>`, or sign in with `aws sso login --profile <profile>` for an SSO profile, then try again.",
	}
	credentialsExpired = Diagnosis{
		Cause:       CauseNoCredentials,
		Explanation: "The storage credentials have expired.",
		Fix:         "Refresh them (for an SSO profile, run `aws sso login --profile <profile>`), then try again.",
	}
	credentialsRejected = Diagnosis{
		Cause:       CauseNoCredentials,
		Explanation: "The storage provider doesn't recognize the access key, or its secret is wrong.",
		Fix:         "Check the access key ID and secret, or create a new key, then try again.",
	}
)

// diagnoseCredentials recognizes a failure to find or use credentials.
func diagnoseCredentials(err error) (Diagnosis, bool) {
	var profileMissing awsconfig.SharedConfigProfileNotExistError
	if errors.As(err, &profileMissing) {
		return Diagnosis{
			Cause:       CauseNoCredentials,
			Explanation: "The AWS profile isn't in ~/.aws/config or ~/.aws/credentials.",
			Fix:         "Create it with `aws configure --profile <profile>`, or choose a profile that exists.",
		}, true
	}
	var ssoToken *ssocreds.InvalidTokenError
	if errors.As(err, &ssoToken) {
		return Diagnosis{
			Cause:       CauseNoCredentials,
			Explanation: "The AWS SSO sign-in for the storage profile has expired.",
			Fix:         "Run `aws sso login --profile <profile>`, then try again.",
		}, true
	}
	if credentials.CredentialProcessFailed(err) {
		return Diagnosis{
			Cause:       CauseNoCredentials,
			Explanation: "The storage profile's credential_process command failed.",
			Fix:         "Run that command yourself to see why, fix it, then try again.",
		}, true
	}
	if errors.Is(err, credentials.ErrMissingCredential) {
		return Diagnosis{
			Cause:       CauseNoCredentials,
			Explanation: "The R2 access key isn't in the Keychain.",
			Fix:         "Run setup again and paste the R2 access key ID and secret.",
		}, true
	}
	var emptyStatic *awscredentials.StaticCredentialsEmptyError
	if errors.As(err, &emptyStatic) || fromMetadataService(err) {
		return noCredentialsFound, true
	}
	return Diagnosis{}, false
}

// fromMetadataService reports whether err passed through a call to the EC2
// instance metadata service. The SDK asks it for credentials only when a
// profile names no other source, so on a Mac such a failure means the
// profile has no credentials, even when the call itself failed to connect.
// The metadata call's error sits inside the storage call's own operation
// error, so each operation error in the chain is checked, not just the
// first.
func fromMetadataService(err error) bool {
	for err != nil {
		var op *smithy.OperationError
		if !errors.As(err, &op) {
			return false
		}
		if op.Service() == "ec2imds" {
			return true
		}
		err = op.Unwrap()
	}
	return false
}

// diagnoseCode recognizes an S3 or R2 API error code. A HEAD response has
// no body to carry one, so for HEAD the SDK uses the HTTP status text
// instead: Forbidden, MovedPermanently, NotFound. Only the status text is
// matched for those, so a 403 whose body names another code (such as
// RequestTimeTooSkewed) is not taken for a refusal.
func diagnoseCode(err error, code string) (Diagnosis, bool) {
	switch code {
	case "ExpiredToken", "ExpiredTokenException", "TokenRefreshRequired":
		return credentialsExpired, true
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidToken", "Unauthorized":
		return credentialsRejected, true
	case "AccessDenied", "AllAccessDisabled", "Forbidden":
		return accessDenied, true
	case "NoSuchBucket":
		return noSuchBucket, true
	case "PermanentRedirect", "MovedPermanently", "AuthorizationHeaderMalformed", "IllegalLocationConstraintException":
		return wrongRegion(bucketRegion(err)), true
	case "NotFound":
		// A 404 names the bucket only for a request about the bucket
		// itself; for an object it is the object that is missing.
		var op *smithy.OperationError
		if errors.As(err, &op) && op.Operation() == "HeadBucket" {
			return noSuchBucket, true
		}
	}
	var missing *types.NoSuchBucket
	if errors.As(err, &missing) {
		return noSuchBucket, true
	}
	return Diagnosis{}, false
}

var (
	accessDenied = Diagnosis{
		Cause:       CauseAccessDenied,
		Explanation: "The storage provider refused access to the bucket.",
		Fix:         "Check that the credentials are current and allowed to list, read, write and delete in the bucket, then try again.",
	}
	noSuchBucket = Diagnosis{
		Cause:       CauseNoSuchBucket,
		Explanation: "The bucket doesn't exist, or its name is misspelled.",
		Fix:         "Check the bucket name, or create the bucket, then try again.",
	}
)

func wrongRegion(region string) Diagnosis {
	if region == "" {
		return Diagnosis{
			Cause:       CauseWrongRegion,
			Explanation: "The bucket is in a different AWS region from the one configured.",
			Fix:         "Set the region to the bucket's region (the S3 console shows it), then try again.",
		}
	}
	return Diagnosis{
		Cause:       CauseWrongRegion,
		Explanation: "The bucket is in " + region + ", not the AWS region configured.",
		Fix:         "Set the region to " + region + ", then try again.",
		Region:      region,
	}
}

// regionPattern matches an AWS region name such as us-east-1 or
// us-gov-west-1, so a header value is never repeated unless it is one.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)

// bucketRegion returns the region S3 names in its x-amz-bucket-region
// response header, or "" when there is none.
func bucketRegion(err error) string {
	var response *smithyhttp.ResponseError
	if !errors.As(err, &response) || response.Response == nil {
		return ""
	}
	region := response.Response.Header.Get("X-Amz-Bucket-Region")
	if !regionPattern.MatchString(region) {
		return ""
	}
	return region
}

// isNetworkError reports whether err means the request never got an
// answer: the connection failed, or the deadline passed first.
func isNetworkError(err error) bool {
	var send *smithyhttp.RequestSendError
	var netErr net.Error
	return errors.As(err, &send) || errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded)
}
