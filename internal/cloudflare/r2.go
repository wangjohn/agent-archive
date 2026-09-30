package cloudflare

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// The permissions guided setup needs, as Cloudflare names them. The
// bootstrap token needs the first two; the token setup creates carries the
// third, on one bucket.
const (
	// PermissionR2Write lets the bootstrap token create buckets and set
	// their lifecycle rules.
	PermissionR2Write = "Workers R2 Storage Write"
	// PermissionTokensWrite lets the bootstrap token create, and revoke,
	// the bucket-scoped token.
	PermissionTokensWrite = "Account API Tokens Write" //nolint:gosec // G101: a permission name, not a credential
	// PermissionBucketItemWrite is the group the bucket-scoped token
	// carries: read, write, and list objects in the buckets it names.
	PermissionBucketItemWrite = "Workers R2 Storage Bucket Item Write"
)

// TokenDocsURL is Cloudflare's page on account-owned API tokens. Cloudflare
// documents the way to the token page (Manage account, then Account API
// tokens) but no dashboard URL for it, so setup names the path and links
// this page rather than guess a deep link.
const TokenDocsURL = "https://developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/" //nolint:gosec // G101: a documentation link, not a credential

// Jurisdictions are the data-residency jurisdictions a bucket can be created
// in, besides the default of none.
var Jurisdictions = []string{"eu", "us", "fedramp", "fedramp-high"}

// LocationHints are the region hints a bucket can be created with.
var LocationHints = []string{"apac", "eeur", "enam", "weur", "wnam", "oc"}

// ValidJurisdiction reports whether j is a jurisdiction.
func ValidJurisdiction(j string) bool { return contains(Jurisdictions, j) }

// ValidLocationHint reports whether h is a location hint.
func ValidLocationHint(h string) bool { return contains(LocationHints, h) }

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// ValidateBucketName checks a name against R2's rules: 3 to 63 characters,
// lowercase letters, digits, and hyphens, none of them leading or trailing.
func ValidateBucketName(name string) error {
	switch {
	case len(name) < 3 || len(name) > 63:
		return errors.New("a bucket name is 3 to 63 characters")
	case !bucketName.MatchString(name):
		return errors.New("a bucket name uses lowercase letters, digits, and hyphens, and neither starts nor ends with a hyphen")
	}
	return nil
}

// Endpoint is the S3 API endpoint of the account's buckets: jurisdictional
// buckets are reachable only at their own host.
func Endpoint(account, jurisdiction string) string {
	if jurisdiction == "" {
		return "https://" + account + ".r2.cloudflarestorage.com"
	}
	return "https://" + account + "." + jurisdiction + ".r2.cloudflarestorage.com"
}

// BucketResource is the token-policy resource string for one bucket:
// com.cloudflare.edge.r2.bucket.<account>_<jurisdiction>_<bucket>, where the
// jurisdiction is "default" for a bucket in none.
func BucketResource(account string, bucket BucketRef) string {
	jurisdiction := bucket.Jurisdiction
	if jurisdiction == "" {
		jurisdiction = "default"
	}
	return fmt.Sprintf("com.cloudflare.edge.r2.bucket.%s_%s_%s", account, jurisdiction, bucket.Name)
}

// SelectPermissionGroup returns the ID of the selectable group called name.
// The ID is looked up at run time and never written into the code:
// Cloudflare documents the ID, not the name, as the stable key.
func SelectPermissionGroup(groups []PermissionGroup, name string) (string, error) {
	found := false
	for _, g := range groups {
		if g.Name != name {
			continue
		}
		found = true
		if g.IsSelectable && g.ID != "" {
			return g.ID, nil
		}
	}
	if found {
		return "", fmt.Errorf("the %q permission group exists but this token may not grant it", name)
	}
	return "", fmt.Errorf("no %q permission group is listed", name)
}

// S3Credentials are the S3-compatible key pair of an account-owned API
// token.
type S3Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// DeriveS3Credentials turns a created token into the S3 key pair R2's
// endpoint accepts: the Access Key ID is the token's ID, and the Secret
// Access Key is the SHA-256 hash of its value.
//
// Cloudflare's documentation says "SHA-256 hash" without spelling out the
// encoding. Lowercase hex of the hash of the value's bytes is what is
// implemented here. That is unconfirmed: guided setup checks the derived key
// against the bucket (a full storage round trip) before it stores anything,
// and the live acceptance step in dev/contributing/testing.md must confirm
// this function against a real account before release. If the encoding is
// wrong, this function is the only place to change.
func DeriveS3Credentials(token Token) S3Credentials {
	sum := sha256.Sum256([]byte(token.Value))
	return S3Credentials{AccessKeyID: token.ID, SecretAccessKey: hex.EncodeToString(sum[:])}
}
