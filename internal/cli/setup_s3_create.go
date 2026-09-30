package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Guided creation of an Amazon S3 bucket in the person's own AWS account
// (dev/proposals/portable-handoff-and-onboarding.md, Part 2b).
//
// It uses the AWS profile chosen for storage, at setup time. That profile
// needs s3:CreateBucket and s3:PutBucketPublicAccessBlock, which the runtime
// policy in docs/security/bucket-permissions.md deliberately lacks, so a
// profile that cannot create buckets falls back to picking an existing one.
// Setup never creates IAM users or keys, and does not set a lifecycle rule
// (see the note on bucket-permissions.md).

// storageChoiceS3New is the storage menu's key for creating an S3 bucket.
const storageChoiceS3New = "s3-new"

// newBucketWord, typed where setup asks for a bucket name of a profile that
// can see none, asks for a new bucket instead. No real bucket can have it: a
// name this short is long taken.
const newBucketWord = "new"

// errWantsNewBucket is promptBucket's answer when the person typed
// newBucketWord.
var errWantsNewBucket = errors.New("a new bucket was asked for")

// BucketCreator creates and secures a bucket in one AWS profile's account.
// It holds the profile's credentials only inside the SDK client.
type BucketCreator interface {
	// CreateBucket creates bucket in region; a name already in use is
	// storage.ErrBucketNameTaken and changes nothing.
	CreateBucket(ctx context.Context, bucket, region string) error
	// BlockPublicAccess turns on all four Block Public Access settings.
	BlockPublicAccess(ctx context.Context, bucket string) error
	// DeleteBucket deletes an empty bucket.
	DeleteBucket(ctx context.Context, bucket string) error
	// InspectPrivacy reads the bucket's public access controls back.
	InspectPrivacy(ctx context.Context, bucket string) storage.PrivacyReport
}

func (e Env) awsBucketCreator(profile, region string) (BucketCreator, error) {
	if e.AWSBucketCreator != nil {
		return e.AWSBucketCreator(profile, region)
	}
	return openAWSBucketCreator(profile, region)
}

// openAWSBucketCreator is Env.AWSBucketCreator's default: a client for
// profile in region, the region the bucket is created in. It makes one
// attempt per call: a CreateBucket whose answer was lost and is retried
// would find its own bucket and read as a taken name. The package's tests
// replace it with one that fails, so a test that leaves Env.AWSBucketCreator
// unset never reaches AWS.
var openAWSBucketCreator = func(profile, region string) (BucketCreator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	cfg, err := credentials.LoadAWSConfig(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return storage.NewBucketAdmin(storage.NewClient(cfg, "", true, 1)), nil
}

// bucketCreateTimeout bounds each call that creates, secures or deletes a
// bucket.
const bucketCreateTimeout = 60 * time.Second

// promptS3Bucket asks for the S3 profile, then for a bucket: a new private
// one when create is set, else an existing one.
func promptS3Bucket(p *prompter, cfg *credentials.Config, env Env, failedRegion string, create bool) error {
	if !create {
		return promptS3Location(p, cfg, env, failedRegion)
	}
	profileRegion, noCredentials, err := chooseS3Profile(p, cfg, env)
	if err != nil {
		return err
	}
	if created, err := createS3Bucket(p, cfg, env, profileRegion, noCredentials); err != nil || created {
		return err
	}
	return promptS3ExistingBucket(p, cfg, env, failedRegion, profileRegion, noCredentials)
}

// createS3Bucket creates a private bucket with the profile in cfg and
// records its name and region in cfg. It returns false, having said why,
// when setup should ask for an existing bucket instead: the profile cannot
// create buckets, or the attempt failed with nothing left to clean up.
func createS3Bucket(p *prompter, cfg *credentials.Config, env Env, profileRegion string, noCredentials bool) (bool, error) {
	profile := cfg.AWSProfile
	if noCredentials {
		terminal.Printf(p.out, "Profile %s has no credentials configured, so it can't create a bucket. Pick an existing bucket instead.\n", profile)
		return false, nil
	}
	terminal.Printf(p.out, "Setup will create a private bucket in the AWS account of profile %s, using that profile now.\n", profile)
	terminal.Println(p.out, "That needs permission to create buckets and set Block Public Access, which day-to-day archiving does not.")
	region, err := promptRegion(p, "Region for the new bucket (for example us-east-1)", firstNonEmpty(cfg.Region, profileRegion))
	if err != nil {
		return false, err
	}
	if partition := awsPartition(region); partition != "aws" {
		terminal.Printf(p.out, "Setup can only create buckets in the standard AWS partition, and %s is in the %s partition. Create the bucket yourself (see the bucket guide) and pick it instead.\n", region, partition)
		return false, nil
	}
	creator, err := env.awsBucketCreator(profile, region)
	if err != nil {
		terminal.Printf(p.out, "Couldn't open profile %s (%s). Pick an existing bucket instead.\n", profile, discoveryReason(err))
		return false, nil
	}
	name, created, err := createNamedBucket(p, creator, profile, region)
	if err != nil || !created {
		return false, err
	}
	if secured, err := secureNewBucket(p, creator, name, profile); err != nil || !secured {
		return false, err
	}
	cfg.Bucket, cfg.Region = name, region
	printRuntimePolicyAdvice(p, name, firstNonEmpty(cfg.Prefix, defaultPrefix))
	return true, nil
}

// awsPartition names the AWS partition a region is in, from its name: "aws"
// (the standard one), "aws-cn", "aws-us-gov", or "aws-iso" for the isolated
// ones. ARNs, and so the runtime policy, differ by partition, and guided
// creation is only built and worded for the standard one.
func awsPartition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	case strings.Contains(region, "-iso"):
		return "aws-iso"
	}
	return "aws"
}

// namesTakenBeforeAsking is how many "name in use" answers in a row setup
// takes before it offers to give up on creating and pick an existing bucket.
// The first answer for a suggested name is retried on its own, so the offer
// comes with the second.
const namesTakenBeforeAsking = 2

// createNamedBucket asks for the bucket's name, defaulting to a random one,
// and creates it. A suggested name that is in use is replaced once by another
// random one; any other name in use is asked for again. After two in-use
// answers in a row every further one offers picking an existing bucket, so
// the loop always has an exit even when S3's answers make every name look
// taken. created is false, with the reason said, when setup should ask for an
// existing bucket instead.
func createNamedBucket(p *prompter, creator BucketCreator, profile, region string) (name string, created bool, err error) {
	suggested := newBucketName()
	if name, err = promptNewBucketName(p, suggested); err != nil {
		return "", false, err
	}
	autoRetried, inUse := false, 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), bucketCreateTimeout)
		err = creator.CreateBucket(ctx, name, region)
		cancel()
		switch {
		case err == nil:
			terminal.Printf(p.out, "Created bucket %s in %s.\n", name, region)
			return name, true, nil
		case errors.Is(err, storage.ErrBucketNameTaken):
			inUse++
			terminal.Printf(p.out, "The name %s is already in use, by you or by someone else (bucket names are shared by everyone on AWS).\n", name)
			if name == suggested && !autoRetried {
				autoRetried = true
				name = newBucketName()
				suggested = name
				terminal.Printf(p.out, "Trying %s instead.\n", name)
				continue
			}
			if inUse >= namesTakenBeforeAsking {
				answer, err := p.menu("What now?", "name",
					option{"name", "Choose another name"},
					option{"existing", "Pick an existing bucket instead"})
				if err != nil || answer == "existing" {
					return "", false, err
				}
			}
			suggested = newBucketName()
			if name, err = promptNewBucketName(p, suggested); err != nil {
				return "", false, err
			}
		case errors.Is(err, storage.ErrInvalidBucketName):
			terminal.Printf(p.out, "S3 doesn't accept the name %s. Choose another.\n", name)
			suggested = newBucketName()
			if name, err = promptNewBucketName(p, suggested); err != nil {
				return "", false, err
			}
		default:
			noteCreateFailure(p, profile, name, err)
			return "", false, nil
		}
	}
}

// promptNewBucketName asks for a bucket name until it is one S3 accepts.
func promptNewBucketName(p *prompter, def string) (string, error) {
	for {
		name, err := p.required("Name for the new bucket", def)
		if err != nil {
			return "", err
		}
		problem := bucketNameProblem(name)
		if problem == "" {
			return name, nil
		}
		terminal.Printf(p.out, "%q can't be a bucket name: %s.\n", name, problem)
	}
}

// noteCreateFailure says in plain words why the bucket called name could not
// be created, and that setup goes on to pick an existing one. It never quotes
// S3's error. When the answer was unclear the bucket may exist, so it says
// where to look and offers nothing that deletes.
func noteCreateFailure(p *prompter, profile, name string, err error) {
	const fallback = " For now, pick an existing bucket instead."
	d := storage.Diagnose(err)
	switch {
	case errors.Is(err, storage.ErrBucketMayExist):
		terminal.Printf(p.out, "S3 didn't answer clearly, so the bucket %s may have been created. Check the S3 console for it, and delete it there if you don't want it; setup won't touch it.%s\n", name, fallback)
	case errors.Is(err, storage.ErrTooManyBuckets):
		terminal.Println(p.out, "This AWS account has reached its limit on buckets. Delete one you don't need, or ask AWS to raise the limit."+fallback)
	case d.Cause == storage.CauseAccessDenied:
		terminal.Printf(p.out, "Profile %s isn't allowed to create buckets. This step needs a profile with s3:CreateBucket and s3:PutBucketPublicAccessBlock (and s3:DeleteBucket to undo a failed attempt);\n", profile)
		terminal.Println(p.out, "an organization policy (a service control policy or permissions boundary) can also forbid it. Use a profile that may create buckets, for this step only."+fallback)
	case d.Cause == storage.CauseWrongRegion:
		// For CreateBucket this is S3 refusing the region, not a bucket in
		// another one.
		terminal.Println(p.out, "S3 didn't accept that region for a new bucket. Check its name, and that your account has the region enabled."+fallback)
	case d.Cause == storage.CauseNoCredentials || d.Cause == storage.CauseNetwork:
		terminal.Println(p.out, "Couldn't create the bucket. "+d.Explanation)
		terminal.Println(p.out, strings.ReplaceAll(d.Fix, "<profile>", profile)+fallback)
	default:
		terminal.Println(p.out, "Couldn't create the bucket: S3 returned an error setup doesn't recognize. Check the region and your AWS account, then run setup again."+fallback)
	}
}

// secureChoice is what the person chose after Block Public Access could not
// be turned on for the new bucket.
type secureChoice string

const (
	secureRetry  secureChoice = "retry"
	secureDelete secureChoice = "delete"
	secureStop   secureChoice = "stop"
)

// secureNewBucket turns on Block Public Access for a bucket this run just
// created and reads it back. Until that works the bucket is not used, so
// nothing is uploaded to it: setup offers to try again, delete the (empty)
// bucket, or stop, and never carries on. It returns false, with a nil
// error, when the bucket was deleted and setup asks for an existing one.
func secureNewBucket(p *prompter, creator BucketCreator, bucket, profile string) (secured bool, err error) {
	for {
		problem := blockPublicAccess(p, creator, bucket)
		if problem == "" {
			return true, nil
		}
		terminal.Printf(p.out, "Bucket %s was created, but %s\n", bucket, problem)
		terminal.Println(p.out, "Setup won't store sessions in it until Block Public Access is on.")
		choice, err := askSecureChoice(p, creator, bucket, profile)
		if err != nil {
			return false, err
		}
		switch choice {
		case secureDelete:
			return false, nil
		case secureStop:
			return false, fmt.Errorf("bucket %s was created but Block Public Access is not on; turn it on in the S3 console, or delete the bucket, then run setup again", bucket)
		case secureRetry:
		}
	}
}

// askSecureChoice asks what to do about a bucket whose Block Public Access
// is not on, and carries out a deletion itself. S3 gives a successful
// CreateBucket no sign that the bucket was created just now rather than
// already being the account's (in us-east-1 it answers both alike), and two
// runs that pick the same name at the same moment can both succeed, so
// deleting is never done on a single keypress: the person types the
// bucket's name to confirm, and S3 refuses to delete one that has objects.
func askSecureChoice(p *prompter, creator BucketCreator, bucket, profile string) (secureChoice, error) {
	for {
		answer, err := p.menu("What now?", string(secureRetry),
			option{string(secureRetry), "Try again"},
			option{string(secureDelete), "Delete the empty bucket and pick an existing one"},
			option{string(secureStop), "Stop setup and leave the bucket as it is"})
		if err != nil {
			return secureStop, err
		}
		if secureChoice(answer) != secureDelete {
			return secureChoice(answer), nil
		}
		typed, err := p.withDefault("Type the bucket name "+bucket+" to delete it, or press Enter to keep it", "")
		if err != nil {
			return secureStop, err
		}
		if typed == bucket {
			deleteNewBucket(p, creator, bucket, profile)
			return secureDelete, nil
		}
		terminal.Printf(p.out, "Not deleted: %q isn't %s.\n", typed, bucket)
	}
}

// Block Public Access is read back a few times, since a bucket that was
// just created can briefly answer "no such bucket" or nothing to the calls
// that follow it.
const bucketSettleAttempts = 3

// bucketSettleDelay is the wait between those attempts. A variable only so
// tests do not wait.
var bucketSettleDelay = time.Second

// blockPublicAccess turns Block Public Access on for bucket and reads it
// back, and returns why not when it could not, or "" on success. A read-back
// the profile may not make (no s3:GetBucketPublicAccessBlock) is a warning,
// not a failure: setup's storage check reads it again with the profile it
// saves. A read-back that shows a setting off is a failure.
func blockPublicAccess(p *prompter, creator BucketCreator, bucket string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*bucketCreateTimeout)
	defer cancel()
	var err error
	for attempt := range bucketSettleAttempts {
		if attempt > 0 {
			time.Sleep(bucketSettleDelay)
		}
		err = creator.BlockPublicAccess(ctx, bucket)
		if err == nil || storage.Diagnose(err).Cause != storage.CauseNoSuchBucket {
			break
		}
	}
	if err != nil {
		if storage.Diagnose(err).Cause == storage.CauseAccessDenied {
			return "setup couldn't turn on Block Public Access: the profile needs s3:PutBucketPublicAccessBlock, and an organization policy can also forbid it."
		}
		return "setup couldn't turn on Block Public Access (" + discoveryReason(err) + ")."
	}
	var report storage.PrivacyReport
	for attempt := range bucketSettleAttempts {
		if attempt > 0 {
			time.Sleep(bucketSettleDelay)
		}
		report = creator.InspectPrivacy(ctx, bucket)
		if report.State != "not_verified" || readBlockPublicAccess(report) {
			break
		}
	}
	if report.State == "public_or_risky" {
		return "it still looks public after Block Public Access was turned on (" + privacyReasonText(report.Reason) + ")."
	}
	if report.State == "verified_private" {
		terminal.Println(p.out, "  "+p.style.okMark()+" Checked: Block Public Access is on for all four settings.")
		return ""
	}
	if readBlockPublicAccess(report) {
		return "Block Public Access reads back with a setting still off."
	}
	p.warn("Block Public Access was turned on, but this profile can't read it back to confirm (that needs s3:GetBucketPublicAccessBlock). Setup checks again at the review.")
	return ""
}

// readBlockPublicAccess reports whether report includes the bucket's Block
// Public Access settings as S3 answered them.
func readBlockPublicAccess(report storage.PrivacyReport) bool {
	return containsString(report.Checks, "bucket_public_access_block")
}

// deleteNewBucket deletes the empty bucket this run created. When S3 refuses,
// it says how to delete the bucket by hand; setup goes on to an existing
// bucket either way.
func deleteNewBucket(p *prompter, creator BucketCreator, bucket, profile string) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketCreateTimeout)
	defer cancel()
	if err := creator.DeleteBucket(ctx, bucket); err != nil {
		terminal.Printf(p.out, "Couldn't delete bucket %s (%s). It is empty; delete it yourself with:\n", bucket, discoveryReason(err))
		terminal.Printf(p.out, "  aws s3api delete-bucket --bucket %s --profile %s\n", bucket, profile)
		return
	}
	terminal.Printf(p.out, "Deleted bucket %s.\n", bucket)
}

// newBucketName is a default bucket name: agent-archive- and eight random
// hex digits, since bucket names are shared by everyone on AWS. Tests replace
// it.
var newBucketName = func() string {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failing is extraordinarily unlikely; the clock still
		// makes a name that a person can change or S3 can refuse.
		return fmt.Sprintf("agent-archive-%x", time.Now().UnixNano()&0xffffffff)
	}
	return "agent-archive-" + hex.EncodeToString(random[:])
}

var (
	bucketNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)
	bucketNameIPv4  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
)

// reservedBucketPrefixes and reservedBucketSuffixes are what S3 refuses at
// the start and end of a general purpose bucket name.
var (
	reservedBucketPrefixes = []string{"xn--", "sthree-", "amzn-s3-demo-"}
	reservedBucketSuffixes = []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3", "-an"}
)

// bucketNameProblem says in a few words why name is not an S3 bucket name,
// or returns "" when it is one. The rules are S3's for general purpose
// buckets: 3 to 63 lowercase letters, digits, hyphens and dots, starting and
// ending with a letter or digit, no adjacent dots, not shaped like an IP
// address, and none of the reserved prefixes and suffixes.
func bucketNameProblem(name string) string {
	switch {
	case len(name) < 3 || len(name) > 63:
		return "it must be 3 to 63 characters long"
	case !bucketNameShape.MatchString(name):
		return "use only lowercase letters, digits, hyphens and dots, starting and ending with a letter or digit"
	case strings.Contains(name, ".."):
		return "it can't have two dots in a row"
	case bucketNameIPv4.MatchString(name):
		return "it can't look like an IP address"
	}
	for _, prefix := range reservedBucketPrefixes {
		if strings.HasPrefix(name, prefix) {
			return "S3 reserves names starting with " + prefix
		}
	}
	for _, suffix := range reservedBucketSuffixes {
		if strings.HasSuffix(name, suffix) {
			return "S3 reserves names ending with " + suffix
		}
	}
	return ""
}

// runtimePolicyTemplate is the runtime policy of
// docs/security/bucket-permissions.md, for the placeholder bucket
// my-archive-bucket and prefix agent-archive. The document is what people
// read and copy; TestRuntimePolicyMatchesDocument fails when this text and
// the document's differ.
const runtimePolicyTemplate = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ArchiveObjects",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-archive-bucket/agent-archive/*"
    },
    {
      "Sid": "ListArchivePrefix",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-archive-bucket",
      "Condition": {"StringLike": {"s3:prefix": ["agent-archive/*"]}}
    },
    {
      "Sid": "InspectBucketPrivacyReadOnly",
      "Effect": "Allow",
      "Action": ["s3:GetBucketPublicAccessBlock", "s3:GetBucketPolicyStatus", "s3:GetBucketAcl"],
      "Resource": "arn:aws:s3:::my-archive-bucket"
    }
  ]
}`

// runtimePolicy is the runtime policy for bucket and prefix.
func runtimePolicy(bucket, prefix string) string {
	return strings.NewReplacer(
		"my-archive-bucket", bucket,
		"agent-archive/*", strings.Trim(prefix, "/")+"/*",
	).Replace(runtimePolicyTemplate)
}

// printRuntimePolicyAdvice prints the least-privilege policy for the new
// bucket and recommends a separate runtime profile, since the profile used
// to create the bucket is usually much broader than archiving needs and is
// what setup saves.
func printRuntimePolicyAdvice(p *prompter, bucket, prefix string) {
	terminal.Println(p.out, "")
	terminal.Println(p.out, p.style.bold("Recommended: archive with a narrower profile"))
	terminal.Println(p.out, p.style.hang("", "Setup will save the profile you just used, which can do far more than archiving needs. Attach this policy to a separate IAM user or role, save its access key as its own AWS profile, then run setup again and choose that profile for storage:"))
	terminal.Println(p.out, "")
	terminal.Println(p.out, runtimePolicy(bucket, prefix))
	terminal.Println(p.out, "")
	terminal.Println(p.out, p.style.dim("This policy hasn't been tested against a real AWS bucket yet. If the storage check fails once you switch to the new profile, see the note in the bucket permissions guide."))
	terminal.Println(p.out, "More: https://github.com/wangjohn/agent-archive/blob/main/docs/security/bucket-permissions.md")
	terminal.Println(p.out, "")
}
