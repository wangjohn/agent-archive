package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Errors BucketAdmin.CreateBucket returns for outcomes a caller acts on. Each
// wraps the provider's error where there is one.
var (
	// ErrBucketNameTaken means S3 would not create a bucket because the name
	// is already in use, in this account or another (bucket names are
	// global), or its state cannot be told apart from that. Nothing was
	// created or changed.
	ErrBucketNameTaken = errors.New("storage: that bucket name is already in use")
	// ErrBucketMayExist means the request to create the bucket got no clear
	// answer (the connection failed, or S3 answered with a server error), so
	// the bucket may or may not have been created. A caller must not delete
	// or reuse it on the strength of this call.
	ErrBucketMayExist = errors.New("storage: the bucket may have been created")
	// ErrTooManyBuckets means the account has reached its bucket limit.
	ErrTooManyBuckets = errors.New("storage: the account has reached its bucket limit")
	// ErrInvalidBucketName means S3 does not accept the name.
	ErrInvalidBucketName = errors.New("storage: S3 does not accept that bucket name")
)

// awsDefaultRegion is the region whose CreateBucket takes no location
// constraint.
const awsDefaultRegion = "us-east-1"

// BucketAdmin creates and secures a new Amazon S3 bucket. It is used at
// setup time only, with the AWS profile the person chose, and needs
// permissions the runtime policy in docs/security/bucket-permissions.md
// deliberately omits (s3:CreateBucket, s3:PutBucketPublicAccessBlock, and
// s3:DeleteBucket to undo a half-finished creation). Its client's region
// must be the region the bucket is created in. It never handles object
// data and never creates IAM users or keys.
type BucketAdmin struct {
	client *s3.Client
	// cfg is the AWS configuration client was made from; the credentials
	// check builds its STS client from it, so STS gets the region, FIPS and
	// dual-stack settings and the endpoint overrides that apply to STS, not
	// the ones that apply to S3.
	cfg aws.Config
}

// NewBucketAdmin returns a BucketAdmin that calls S3 with client, which was
// made from cfg (see NewClient).
func NewBucketAdmin(client *s3.Client, cfg aws.Config) *BucketAdmin {
	return &BucketAdmin{client: client, cfg: cfg}
}

// CreateBucket creates a bucket named name in region, with a location
// constraint everywhere except us-east-1, where S3 refuses one.
//
// It first asks whether the name exists at all (a HEAD request, answered
// 404 only for a name nobody owns), in every region. In us-east-1 that is
// what stops setup changing a bucket it did not create: S3 answers a request
// to create a bucket its caller already owns there with success and resets
// its ACLs. Elsewhere it spares a same-name bucket the caller owns in
// us-east-1 a confusing result, and narrows (does not close) the window in
// which two runs choosing the same name can both succeed: S3 gives a
// successful CreateBucket no signal that says "created just now" as opposed
// to "already yours" in us-east-1, so callers must treat a bucket this call
// created as identified by the name alone.
//
// An existing name, whoever owns it, is ErrBucketNameTaken and is left
// untouched. A 403 or 301 to the HEAD request reads as taken only once the
// credentials are shown to work (see checkCredentials): a refused key gets 403
// too, and would otherwise make every name look taken. A request that gets
// no clear answer is ErrBucketMayExist.
func (a *BucketAdmin) CreateBucket(ctx context.Context, name, region string) error {
	if err := a.checkNameFree(ctx, name); err != nil {
		return err
	}
	var constraint *types.CreateBucketConfiguration
	if region != awsDefaultRegion {
		constraint = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	_, err := a.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name), CreateBucketConfiguration: constraint})
	if err != nil {
		return classifyCreateError(err)
	}
	return nil
}

// checkNameFree returns nil when no bucket has the name, ErrBucketNameTaken
// when one does, and the error itself when the answer is unclear.
func (a *BucketAdmin) checkNameFree(ctx context.Context, name string) error {
	_, err := a.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
	switch {
	case err == nil:
		return ErrBucketNameTaken
	case isMissingBucket(err):
		return nil
	case isBucketOwnedElsewhere(err):
		if credentialsErr := a.checkCredentials(ctx); credentialsErr != nil {
			return credentialsErr
		}
		return ErrBucketNameTaken
	}
	return err
}

// credentialsCheckError is a failed credentials check: the client's
// credentials are refused, expired, missing, or could not be checked.
type credentialsCheckError struct{ err error }

func (e *credentialsCheckError) Error() string { return "storage: check credentials: " + e.err.Error() }

func (e *credentialsCheckError) Unwrap() error { return e.err }

// checkCredentials asks STS who the client's credentials belong to, a call
// that needs no permission, so it fails only when the credentials do not
// work. The STS client comes from the same AWS configuration as the S3 one,
// so it takes the configuration's global endpoint override (AWS_ENDPOINT_URL
// or a profile's endpoint_url), a service-specific one for STS
// (AWS_ENDPOINT_URL_STS), and the FIPS and dual-stack settings, and never an
// override meant for S3 alone. It makes one attempt: a check that retries
// server errors would hold up setup for no better an answer.
func (a *BucketAdmin) checkCredentials(ctx context.Context) error {
	client := sts.NewFromConfig(a.cfg, func(o *sts.Options) {
		o.HTTPClient = withTimeouts(a.cfg.HTTPClient)
		o.Logger = logging.Nop{}
		o.Retryer = aws.NopRetryer{}
	})
	if _, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return &credentialsCheckError{err: err}
	}
	return nil
}

// diagnoseCredentialsCheck explains a failed credentials check in the terms
// of the storage check's credential diagnoses.
func diagnoseCredentialsCheck(check *credentialsCheckError) Diagnosis {
	var apiErr smithy.APIError
	if errors.As(check.err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "ExpiredToken", "ExpiredTokenException":
			return credentialsExpired
		case "InvalidClientTokenId", "SignatureDoesNotMatch", "InvalidAccessKeyId", "InvalidToken":
			return credentialsRejected
		}
	}
	return Diagnose(check.err)
}

// classifyCreateError turns a CreateBucket error into the sentinel a caller
// acts on, keeping the original error in the chain; any other error is
// returned as is.
func classifyCreateError(err error) error {
	if bucketNameInUse(err) {
		return fmt.Errorf("%w: %w", ErrBucketNameTaken, err)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "TooManyBuckets":
			return fmt.Errorf("%w: %w", ErrTooManyBuckets, err)
		case "InvalidBucketName":
			return fmt.Errorf("%w: %w", ErrInvalidBucketName, err)
		}
	}
	if answerUnclear(err) {
		return fmt.Errorf("%w: %w", ErrBucketMayExist, err)
	}
	return err
}

// answerUnclear reports whether a request may have been carried out although
// it returned err: the connection failed or timed out after it was sent, or
// S3 answered with a server error. A failure to get credentials is not
// unclear: no request was sent.
func answerUnclear(err error) bool {
	if credentialService(err) != "" {
		return false
	}
	if isNetworkError(err) {
		return true
	}
	var response *smithyhttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() >= http.StatusInternalServerError
}

// ReadBlockPublicAccess reads bucket's Block Public Access settings back. On
// success allOn says whether all four are on; the error, when there is
// one, is why they could not be read (s3:GetBucketPublicAccessBlock, a
// bucket S3 does not show yet). It needs s3:GetBucketPublicAccessBlock.
func (a *BucketAdmin) ReadBlockPublicAccess(ctx context.Context, bucket string) (allOn bool, err error) {
	output, err := a.client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String(bucket)})
	if err != nil {
		return false, err
	}
	b := output.PublicAccessBlockConfiguration
	if b == nil {
		return false, nil
	}
	return aws.ToBool(b.BlockPublicAcls) && aws.ToBool(b.IgnorePublicAcls) && aws.ToBool(b.BlockPublicPolicy) && aws.ToBool(b.RestrictPublicBuckets), nil
}

// BlockPublicAccess turns on all four Block Public Access settings for
// bucket. It needs s3:PutBucketPublicAccessBlock.
func (a *BucketAdmin) BlockPublicAccess(ctx context.Context, bucket string) error {
	_, err := a.client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	return err
}

// DeleteBucket deletes bucket, which must be empty (S3 refuses otherwise).
// It needs s3:DeleteBucket.
func (a *BucketAdmin) DeleteBucket(ctx context.Context, bucket string) error {
	_, err := a.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
	return err
}

// InspectPrivacy reads bucket's public access controls with the same
// inspection the collector and status use (S3Store.InspectPrivacy), so
// setup can show a verified result right after it turned Block Public
// Access on.
func (a *BucketAdmin) InspectPrivacy(ctx context.Context, bucket string) PrivacyReport {
	store, err := NewS3Store(S3StoreOptions{Provider: "s3", Client: a.client, Bucket: bucket})
	if err != nil {
		return UnknownPrivacy("s3")
	}
	return store.InspectPrivacy(ctx)
}

// isMissingBucket reports whether a HeadBucket error is S3's 404, which it
// gives only for a name no one owns.
func isMissingBucket(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var response *smithyhttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}

// isBucketOwnedElsewhere reports whether a HeadBucket error may show that
// the name exists: 403 (another account's bucket, one this profile cannot
// list, or credentials S3 refuses) or 301 (a bucket in another region).
func isBucketOwnedElsewhere(err error) bool {
	var response *smithyhttp.ResponseError
	if !errors.As(err, &response) {
		return false
	}
	status := response.HTTPStatusCode()
	return status == http.StatusForbidden || status == http.StatusMovedPermanently
}

// bucketNameInUse reports whether a CreateBucket error says the name is
// unavailable: taken by another account, already owned by the caller, or a
// conflicting operation on it (a recent delete of the same name) is still
// settling.
func bucketNameInUse(err error) bool {
	var exists *types.BucketAlreadyExists
	var owned *types.BucketAlreadyOwnedByYou
	if errors.As(err, &exists) || errors.As(err, &owned) {
		return true
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou", "OperationAborted":
		return true
	}
	return false
}
