package storage

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/wangjohn/agent-archive/internal/destination"
)

// PrivacyReport is the pure persisted observation value.
type PrivacyReport = destination.PrivacyReport

// UnknownPrivacy returns the report for a bucket whose public access has not
// been inspected: state "not_verified", with a reason and guidance link for
// provider. For "r2" the reason says management credentials are not
// configured, since R2 object credentials cannot inspect public access.
func UnknownPrivacy(provider string) PrivacyReport {
	reason := "inspection_unavailable"
	guidanceURL := "https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html"
	if provider == "r2" {
		reason = "r2_management_credentials_not_configured"
		guidanceURL = "https://developers.cloudflare.com/r2/buckets/public-buckets/"
	}
	return PrivacyReport{State: "not_verified", Reason: reason, Scope: "native_bucket_public_access", GuidanceURL: guidanceURL}
}

// granteeGroupURI identifies an S3 predefined grantee group in a bucket ACL.
type granteeGroupURI string

// The predefined groups whose grants make a bucket public.
const (
	allUsersGroupURI           granteeGroupURI = "http://acs.amazonaws.com/groups/global/AllUsers"
	authenticatedUsersGroupURI granteeGroupURI = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
)

// InspectPrivacy makes at most three read-only API calls with the caller's
// credentials. R2's S3 object credentials cannot inspect Cloudflare-managed
// public domains. Never send them to the management API or request admin access.
func (s *S3Store) InspectPrivacy(ctx context.Context) PrivacyReport {
	report := UnknownPrivacy(s.provider)
	if s.provider != "s3" {
		return report
	}
	block, err := s.client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String(s.bucket)})
	if err == nil && block.PublicAccessBlockConfiguration != nil {
		report.Checks = append(report.Checks, "bucket_public_access_block")
		b := block.PublicAccessBlockConfiguration
		if aws.ToBool(b.BlockPublicAcls) && aws.ToBool(b.IgnorePublicAcls) && aws.ToBool(b.BlockPublicPolicy) && aws.ToBool(b.RestrictPublicBuckets) {
			report.State = "verified_private"
			report.Reason = "all_bucket_public_access_blocks_enabled"
			return report
		}
	}
	policy, err := s.client.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String(s.bucket)})
	if err == nil && policy.PolicyStatus != nil {
		report.Checks = append(report.Checks, "bucket_policy_status")
		if aws.ToBool(policy.PolicyStatus.IsPublic) {
			report.State = "public_or_risky"
			report.Reason = "public_bucket_policy"
			return report
		}
	}
	acl, err := s.client.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		report.Checks = append(report.Checks, "bucket_acl")
		for _, grant := range acl.Grants {
			if grant.Grantee == nil {
				continue
			}
			uri := granteeGroupURI(aws.ToString(grant.Grantee.URI))
			if uri == allUsersGroupURI || uri == authenticatedUsersGroupURI {
				report.State = "public_or_risky"
				report.Reason = "public_bucket_acl"
				return report
			}
		}
	}
	// A private bucket ACL/policy cannot rule out public object ACLs or access
	// point policies. Account-level protection may exist but is not inspected.
	report.Reason = "public_access_controls_not_fully_verified"
	return report
}
