package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ErrBucketNameTaken means S3 would not create a bucket because the name is
// already in use, in this account or another. S3 bucket names are global.
// Nothing was created or changed.
var ErrBucketNameTaken = errors.New("storage: that bucket name is already taken")

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
type BucketAdmin struct{ client *s3.Client }

// NewBucketAdmin returns a BucketAdmin that calls S3 with client.
func NewBucketAdmin(client *s3.Client) *BucketAdmin { return &BucketAdmin{client: client} }

// CreateBucket creates a bucket named name in region, with a location
// constraint everywhere except us-east-1, where S3 refuses one. A name that
// is already in use, whoever owns it, is ErrBucketNameTaken and leaves the
// existing bucket untouched: in us-east-1 S3 answers a request to create a
// bucket its caller already owns with success and resets its ACLs, so
// setup would go on to change a bucket it did not create. That region is
// therefore asked first whether the name exists at all (a HEAD request,
// answered 404 only for a name nobody owns), and anything but that 404 is
// taken. Other regions answer BucketAlreadyOwnedByYou, which needs no
// such check.
func (a *BucketAdmin) CreateBucket(ctx context.Context, name, region string) error {
	if region == awsDefaultRegion {
		_, err := a.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)})
		switch {
		case err == nil, isBucketOwnedElsewhere(err):
			return ErrBucketNameTaken
		case !isMissingBucket(err):
			return err
		}
	}
	var constraint *types.CreateBucketConfiguration
	if region != awsDefaultRegion {
		constraint = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(region)}
	}
	_, err := a.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name), CreateBucketConfiguration: constraint})
	if err != nil && bucketNameInUse(err) {
		return fmt.Errorf("%w: %w", ErrBucketNameTaken, err)
	}
	return err
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

// DeleteBucket deletes bucket, which must be empty. It needs
// s3:DeleteBucket.
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

// isBucketOwnedElsewhere reports whether a HeadBucket error shows that the
// name exists: 403 (another account's bucket, or one this profile cannot
// list) or 301 (a bucket in another region).
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
