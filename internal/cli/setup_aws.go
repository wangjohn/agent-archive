package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// AWSProfile contains only the information needed to offer a profile in setup.
// Discovery never retrieves credentials, runs credential_process, or logs in.
type AWSProfile struct {
	Name   string
	Region string
	// NoCredentials is true when the profile's settings name no credential
	// source (no access keys, credential_process, SSO, login session, or
	// role with a source to assume it from) or the SDK cannot load it.
	// Discovery checks only that such a setting is present.
	NoCredentials bool
}

func (e Env) awsProfiles() ([]AWSProfile, error) {
	if e.AWSProfiles != nil {
		return e.AWSProfiles()
	}
	home, err := e.userHomeDir()
	if err != nil {
		return nil, err
	}
	// The same files the collector's LaunchAgent is given (see
	// collectorEnvironment).
	return readAWSProfiles(awsFiles(home, e.lookupEnv))
}

func readAWSProfiles(configPath, credentialsPath string) ([]AWSProfile, error) {
	names := map[string]bool{}
	for i, path := range []string{configPath, credentialsPath} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// This scan uses only section names. The SDK parses the values below, but
			// discovery keeps only the region and whether a credential setting is present.
			if !strings.HasPrefix(line, "[") {
				continue
			}
			end := strings.Index(line, "]")
			if end < 0 {
				continue
			}
			name := strings.TrimSpace(line[1:end])
			if i == 0 {
				if name != "default" && !strings.HasPrefix(name, "profile ") {
					continue
				}
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			}
			if name != "" {
				names[name] = true
			}
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
	}
	profiles := make([]AWSProfile, 0, len(names))
	for name := range names {
		cfg, err := awsconfig.LoadSharedConfigProfile(context.Background(), name, func(o *awsconfig.LoadSharedConfigOptions) {
			o.ConfigFiles = []string{configPath}
			o.CredentialsFiles = []string{credentialsPath}
		})
		if err != nil {
			// The SDK cannot load this profile (a broken source_profile
			// chain, a missing sso-session section, conflicting credential
			// settings), so it cannot supply credentials either.
			profiles = append(profiles, AWSProfile{Name: name, NoCredentials: true})
			continue
		}
		profiles = append(profiles, AWSProfile{Name: name, Region: cfg.Region, NoCredentials: !hasCredentialSource(cfg)})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// hasCredentialSource reports whether a profile names any way to get
// credentials. It looks only at which settings are present; nothing is
// retrieved or run. A role counts only with something to assume it from.
func hasCredentialSource(cfg awsconfig.SharedConfig) bool {
	role := cfg.RoleARN != "" && (cfg.SourceProfileName != "" || cfg.CredentialSource != "" || cfg.WebIdentityTokenFile != "")
	return cfg.Credentials.HasKeys() || cfg.CredentialProcess != "" || cfg.WebIdentityTokenFile != "" ||
		role || cfg.SSOSessionName != "" || cfg.SSOStartURL != "" || cfg.LoginSession != ""
}

// usableAWSProfiles names the discovered profiles that have a credential
// source.
func usableAWSProfiles(profiles []AWSProfile) []string {
	var names []string
	for _, profile := range profiles {
		if !profile.NoCredentials {
			names = append(names, profile.Name)
		}
	}
	return names
}

// defaultAWSProfile is the profile setup offers first: the saved one, then
// AWS_PROFILE, then "default" or the only profile, if it has a credential
// source.
func defaultAWSProfile(saved string, profiles []AWSProfile, env Env) string {
	if saved != "" {
		return saved
	}
	if name := lookupEnvTrimmed(env, "AWS_PROFILE"); name != "" {
		return name
	}
	usable := usableAWSProfiles(profiles)
	if containsString(usable, "default") {
		return "default"
	}
	if len(usable) == 1 {
		return usable[0]
	}
	return ""
}

// defaultStorageProvider is the provider setup offers when none is saved:
// S3 when AWS_PROFILE is set or a discovered profile has a credential
// source, otherwise R2, which needs nothing installed. AWS_PROFILE counts
// even when discovery finds no credential source for it, because the
// profile question defaults to it too (see defaultAWSProfile): its
// credentials can come from somewhere discovery does not look, and setting
// it says the person uses AWS.
func defaultStorageProvider(env Env) string {
	if lookupEnvTrimmed(env, "AWS_PROFILE") != "" {
		return credentials.ProviderS3
	}
	if profiles, err := env.awsProfiles(); err == nil && len(usableAWSProfiles(profiles)) > 0 {
		return credentials.ProviderS3
	}
	return credentials.ProviderR2
}

// BucketFinder lists buckets and reads a bucket's region for one AWS
// profile. It holds the profile's credentials only inside the SDK client and
// never returns them.
type BucketFinder interface {
	Buckets(ctx context.Context) ([]string, error)
	Region(ctx context.Context, bucket string) (string, error)
}

func (e Env) awsBuckets(profile, region string) (BucketFinder, error) {
	if e.AWSBuckets != nil {
		return e.AWSBuckets(profile, region)
	}
	return openAWSBuckets(profile, region)
}

// openAWSBuckets is Env.AWSBuckets' default: a client for profile that asks
// S3 itself. The package's tests replace it with one that fails, so a test
// that leaves Env.AWSBuckets unset never reaches AWS.
var openAWSBuckets = func(profile, region string) (BucketFinder, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	// Listing works from any region; the profile's is used when it has one.
	cfg, err := credentials.LoadAWSConfig(ctx, profile, firstNonEmpty(region, "us-east-1"))
	if err != nil {
		return nil, err
	}
	return s3BucketFinder{storage.NewClient(cfg, "", true, 1)}, nil
}

// bucketDiscoveryTimeout bounds each discovery call, so a profile whose
// credentials never arrive leaves setup asking for the name instead of
// waiting.
const bucketDiscoveryTimeout = 20 * time.Second

type s3BucketFinder struct{ client *s3.Client }

func (f s3BucketFinder) Buckets(ctx context.Context) ([]string, error) {
	return storage.ListBucketNames(ctx, f.client)
}

func (f s3BucketFinder) Region(ctx context.Context, bucket string) (string, error) {
	return storage.BucketRegion(ctx, f.client, bucket)
}

// regionPattern matches AWS region names such as us-east-1,
// us-gov-west-1 and ap-southeast-4.
var regionPattern = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+)+-[0-9]{1,2}$`)

// validRegion reports whether region is shaped like an AWS region name.
func validRegion(region string) bool {
	return regionPattern.MatchString(region)
}

// promptRegion asks for a bucket region until the answer is shaped like
// one, so a path or a typo is never saved as the region.
func promptRegion(p *prompter, label, def string) (string, error) {
	for {
		region, err := p.required(label, def)
		if err != nil || validRegion(region) {
			return region, err
		}
		terminal.Printf(p.out, "%q isn't an AWS region. Enter one like us-east-1 or eu-west-2.\n", region)
		def = ""
	}
}

// promptS3Location asks for the AWS profile, then the bucket, then settles
// the region. With the profile chosen, setup lists its buckets to pick from
// and reads the chosen bucket's own region, so a bucket outside the
// profile's region works. When S3 refuses either lookup, setup says why in
// one line and asks instead.
func promptS3Location(p *prompter, cfg *credentials.Config, env Env) error {
	profiles, err := env.awsProfiles()
	if err != nil {
		terminal.Println(p.out, "Could not read AWS profiles automatically. Enter an existing profile name below.")
	}
	def := defaultAWSProfile(cfg.AWSProfile, profiles, env)
	var profile string
	if len(profiles) > 0 {
		profile, err = pickAWSProfile(p, profiles, def)
	} else {
		profile, err = p.required("AWS profile", def)
	}
	if err != nil {
		return err
	}
	if profile != cfg.AWSProfile {
		cfg.Region = ""
	}
	cfg.AWSProfile = profile
	var profileRegion string
	noCredentials := false
	for _, candidate := range profiles {
		if candidate.Name == profile {
			profileRegion, noCredentials = candidate.Region, candidate.NoCredentials
			break
		}
	}

	var finder BucketFinder
	if noCredentials {
		terminal.Printf(p.out, "Profile %s has no credentials configured, so type the bucket name.\n", profile)
	} else if finder, err = env.awsBuckets(profile, firstNonEmpty(cfg.Region, profileRegion)); err != nil {
		finder = nil
		terminal.Printf(p.out, "Couldn't list the buckets of profile %s (%s), so type the name.\n", profile, discoveryReason(err))
	}
	if cfg.Bucket, err = promptBucket(p, finder, profile, cfg.Bucket); err != nil {
		return err
	}

	if finder != nil {
		region, err := bucketRegion(finder, cfg.Bucket)
		if err == nil {
			cfg.Region = region
			terminal.Printf(p.out, "Bucket %s is in %s; using that region.\n", cfg.Bucket, region)
			return nil
		}
		terminal.Printf(p.out, "Couldn't look up the region of bucket %s (%s).\n", cfg.Bucket, discoveryReason(err))
	}
	if !validRegion(cfg.Region) {
		cfg.Region = ""
	}
	if cfg.Region == "" && validRegion(profileRegion) {
		cfg.Region = profileRegion
	}
	if cfg.Region == "" {
		cfg.Region, err = promptRegion(p, "Bucket region (for example us-east-1)", "")
		return err
	}
	terminal.Printf(p.out, "Using region %s. You can change it at the final review.\n", cfg.Region)
	return nil
}

// promptBucket offers the buckets finder lists by number, or asks for the
// name when there is no finder, the listing fails, or it is empty. The
// saved bucket is the default, else the first named like agent-archive*.
func promptBucket(p *prompter, finder BucketFinder, profile, saved string) (string, error) {
	if finder == nil {
		return p.required("Bucket name", saved)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	names, err := finder.Buckets(ctx)
	cancel()
	if err != nil {
		terminal.Printf(p.out, "Couldn't list the buckets of profile %s (%s), so type the name.\n", profile, discoveryReason(err))
		return p.required("Bucket name", saved)
	}
	if len(names) == 0 {
		terminal.Printf(p.out, "Profile %s can see no buckets, so type the name.\n", profile)
		return p.required("Bucket name", saved)
	}
	def := saved
	if def == "" {
		for _, name := range names {
			if strings.HasPrefix(name, "agent-archive") {
				def = name
				break
			}
		}
	}
	return pickByNumber(p, "Which bucket should sessions be stored in?", names, nil, def, "bucket name")
}

// bucketRegion reads bucket's region. A region S3 names while refusing
// the lookup (a wrong_region failure) is used too.
func bucketRegion(finder BucketFinder, bucket string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	region, err := finder.Region(ctx, bucket)
	if err != nil {
		if d := storage.Diagnose(err); d.Cause == storage.CauseWrongRegion && validRegion(d.Region) {
			return d.Region, nil
		}
		return "", err
	}
	if !validRegion(region) {
		return "", errUnexpectedRegion
	}
	return region, nil
}

var errUnexpectedRegion = errors.New("the answer from S3 is not a region name")

// discoveryReason says in a few words why a bucket lookup failed. Like
// storage.Diagnose, it never quotes the error, which can hold whatever a
// credential_process printed.
func discoveryReason(err error) string {
	if errors.Is(err, errUnexpectedRegion) {
		return "S3 didn't name a region"
	}
	switch storage.Diagnose(err).Cause {
	case storage.CauseAccessDenied:
		return "access denied"
	case storage.CauseNoCredentials:
		return "no working credentials"
	case storage.CauseNoSuchBucket:
		return "no such bucket"
	case storage.CauseNetwork:
		return "S3 couldn't be reached"
	default:
		return "S3 returned an error"
	}
}

// pickAWSProfile lists the discovered profiles by number, marking those with
// no credential source. A profile that discovery missed can still be typed
// by name.
func pickAWSProfile(p *prompter, profiles []AWSProfile, def string) (string, error) {
	names := make([]string, len(profiles))
	notes := make([]string, len(profiles))
	for i, profile := range profiles {
		names[i] = profile.Name
		if profile.NoCredentials {
			notes[i] = p.style.dim("(no credentials configured)")
		}
	}
	return pickByNumber(p, "Which AWS profile has access to the bucket?", names, notes, def, "profile name")
}

// maxListed caps how many choices pickByNumber lists; the rest can be typed.
const maxListed = 20

// pickByNumber lists names by number under question, each followed by its
// note if any, and returns the one chosen by number or the name typed. The
// default is def's number when listed, else def itself.
func pickByNumber(p *prompter, question string, names, notes []string, def, kind string) (string, error) {
	terminal.Println(p.out, question)
	listed := names
	if len(listed) > maxListed {
		listed = listed[:maxListed]
	}
	defNum := def
	for i, name := range listed {
		note := ""
		if i < len(notes) && notes[i] != "" {
			note = " " + notes[i]
		}
		terminal.Printf(p.out, "  %d) %s%s\n", i+1, name, note)
		if name == def {
			defNum = strconv.Itoa(i + 1)
		}
	}
	if more := len(names) - len(listed); more > 0 {
		terminal.Printf(p.out, "  (%d more not listed)\n", more)
	}
	label := fmt.Sprintf("Enter 1-%d, or another %s", len(listed), kind)
	for {
		answer, err := p.withDefault(label, defNum)
		if err != nil {
			return "", err
		}
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(listed) {
			return listed[n-1], nil
		}
		if answer != "" {
			return answer, nil
		}
		terminal.Println(p.out, "This value is required.")
	}
}
