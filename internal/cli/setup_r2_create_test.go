package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// bootstrapCanary stands for the pasted Cloudflare API token. It must never
// show up anywhere but the request that carries it to the fake.
const bootstrapCanary = "CANARY-cf-bootstrap-token-7f3a91"

// trackedAPI is a cloudflare.API that notes when it was discarded.
type trackedAPI struct {
	cloudflare.API
	discarded bool
}

func (a *trackedAPI) Discard() {
	a.discarded = true
	a.API.Discard()
}

// guidedR2Fixture is a first setup in ~/src/web-app with a fake Cloudflare.
type guidedR2Fixture struct {
	*screenFixture
	cf       *cloudflaretest.Server
	keychain *fakeKeychain
	apis     []*trackedAPI
	pauses   int
}

func newGuidedR2Fixture(t *testing.T) *guidedR2Fixture {
	t.Helper()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	g := &guidedR2Fixture{screenFixture: f, cf: cloudflaretest.New(t, bootstrapCanary), keychain: newFakeKeychain()}
	f.env.Keychain = func() (credentials.CredentialStore, error) { return g.keychain, nil }
	f.env.Pause = func(time.Duration) { g.pauses++ }
	f.env.Cloudflare = func(token string) cloudflare.API {
		api := &trackedAPI{API: cloudflare.New(token, cloudflare.Options{
			BaseURL: g.cf.URL + "/client/v4",
			Sleep:   func(context.Context, time.Duration) error { return nil },
		})}
		g.apis = append(g.apis, api)
		return api
	}
	return g
}

// answers is what a person types: the combined apps-and-project question,
// the storage menu's guided choice, then the rest.
func guidedAnswers(rest ...string) string {
	return strings.Join(append([]string{"", "r2-create"}, rest...), "\n") + "\n"
}

// Each answer of the flow that follows the token: the bucket name (default),
// no data location, yes to the retention rule, go ahead, and start archiving.
var (
	askToken     = []string{bootstrapCanary}
	acceptedRest = []string{"", "n", "y", "", ""}
)

func (g *guidedR2Fixture) happy() string {
	return guidedAnswers(append(append([]string{}, askToken...), acceptedRest...)...)
}

func (g *guidedR2Fixture) run(t *testing.T, input string, want int) string {
	t.Helper()
	var out bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(input), &out, &out, g.env); code != want {
		t.Fatalf("setup exit %d, want %d\n%s", code, want, &out)
	}
	return out.String()
}

func (g *guidedR2Fixture) savedConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, found, err := config.Load(g.home)
	if err != nil || !found {
		t.Fatalf("no saved configuration: %v", err)
	}
	return cfg
}

func (g *guidedR2Fixture) notSaved(t *testing.T) {
	t.Helper()
	if _, found, err := config.Load(g.home); err != nil || found {
		t.Fatalf("a configuration was saved: %v %v", found, err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// assertNothingHolds fails if any secret shows in output or in any file the
// run left under the test's Mac: the data directory (configuration, setup
// draft, journal, state), hook files, and the LaunchAgent.
func (g *guidedR2Fixture) assertNothingHolds(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Errorf("output holds %q", secret)
		}
	}
	files := 0
	err := filepath.WalkDir(g.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return nil
		}
		files++
		for _, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %q", path, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("the search found no files, so it proved nothing")
	}
}

var defaultBucketName = regexp.MustCompile(`^agent-archive-[0-9a-f]{6}$`)

func routes(reqs []cloudflaretest.Request) []string {
	var names []string
	for _, r := range reqs {
		names = append(names, string(r.Route))
	}
	return names
}

// The whole flow: the bucket, a token limited to it, the key derived from the
// token and stored as a pasted key would be, the retention rule, and the
// public-access reads, in that order, with setup finishing on the stored key.
func TestGuidedR2CreatesBucketAndScopedKey(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	want := []string{
		string(cloudflaretest.RouteAccounts), string(cloudflaretest.RouteCreateBucket), string(cloudflaretest.RoutePermissionGroups),
		string(cloudflaretest.RouteCreateToken), string(cloudflaretest.RouteLifecycle), string(cloudflaretest.RouteManagedDomain), string(cloudflaretest.RouteCustomDomains),
	}
	if got := routes(g.cf.Requests()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v, want %v", got, want)
	}
	cfg := g.savedConfig(t)
	if cfg.Storage.Provider != credentials.ProviderR2 || !defaultBucketName.MatchString(cfg.Storage.Bucket) ||
		cfg.Storage.R2AccountID != cloudflaretest.AccountID || cfg.Storage.R2Endpoint != "https://"+cloudflaretest.AccountID+".r2.cloudflarestorage.com" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
	tokens := g.cf.Tokens()
	if len(tokens) != 1 || len(g.cf.Live()) != 1 {
		t.Fatalf("tokens %+v", tokens)
	}
	issued := tokens[0]
	// The stored key is the token's ID and the hash of its value.
	stored, err := g.keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || stored.AccessKeyID != issued.ID || stored.SecretAccessKey != sha256Hex(issued.Value) {
		t.Fatalf("stored key %+v (%v), want ID %s", stored, err, issued.ID)
	}
	if len(g.keychain.items) != 1 {
		t.Fatalf("the Keychain holds %d items, want the one key", len(g.keychain.items))
	}
	// The token has one allow policy, on this bucket only, with the group ID
	// Cloudflare listed, and no expiry.
	body, _ := json.Marshal(issued.Body)
	resource := "com.cloudflare.edge.r2.bucket." + cloudflaretest.AccountID + "_default_" + cfg.Storage.Bucket
	if !strings.Contains(string(body), `"`+resource+`":"*"`) || !strings.Contains(string(body), `"id":"aaaa0000000000000000000000000002"`) || strings.Contains(string(body), "expires_on") {
		t.Fatalf("token request %s", body)
	}
	if !strings.HasPrefix(issued.Name, "agent-archive "+cfg.Storage.Bucket+" ") || !strings.Contains(out, `named "`+issued.Name+`"`) {
		t.Fatalf("token name %q; output:\n%s", issued.Name, out)
	}
	// Retention: a delete-after-age rule with an empty prefix.
	if !strings.Contains(fmt.Sprint(g.cf.Lifecycle(cfg.Storage.Bucket)), "maxAge:7.776e+06") {
		t.Fatalf("lifecycle %v", g.cf.Lifecycle(cfg.Storage.Bucket))
	}
	for _, api := range g.apis {
		if !api.discarded {
			t.Fatal("the bootstrap token was not discarded")
		}
	}
	for _, want := range []string{"Created bucket " + cfg.Storage.Bucket, "r2.dev public access: off (checked at setup).", "Custom domains: none (checked at setup).", "not saved anywhere", "Connected to your storage."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "is private") || strings.Contains(out, "Bucket is private") {
		t.Fatalf("claims more than was checked:\n%s", out)
	}
}

// The instructions come before anything is asked, name the permissions, and
// link Cloudflare's page for the token rather than a guessed dashboard link.
func TestGuidedR2ExplainsTheTokenBeforeAskingForIt(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	intro := strings.Index(out, "Workers R2 Storage Write")
	ask := strings.Index(out, "Cloudflare API token (hidden")
	if intro < 0 || ask < intro || !strings.Contains(out, "Account API Tokens Write") || !strings.Contains(out, cloudflare.TokenDocsURL) {
		t.Fatalf("instructions:\n%s", out)
	}
	if strings.Contains(out, "dash.cloudflare.com") {
		t.Fatalf("a dashboard link was guessed:\n%s", out)
	}
}

// The bootstrap token, and the value of the token setup made, are in no file
// the run wrote, no output, and no setup draft, even when setup stops midway.
func TestGuidedR2NeverPersistsTheBootstrapToken(t *testing.T) {
	t.Parallel()
	t.Run("finished setup", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		out := g.run(t, g.happy(), 0)
		issued := g.cf.Tokens()[0]
		g.assertNothingHolds(t, out, bootstrapCanary, issued.Value, "Bearer")
	})
	t.Run("stopped with a saved draft", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		// A check that keeps failing leaves the setup draft on disk.
		g.env.OpenStore = failingOpener(func() error { return invalidKey() })
		input := guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...)
		out := g.run(t, input, 1)
		if _, err := os.Stat(draftPath(g.home)); err != nil {
			t.Fatalf("no setup draft was saved, so the search would prove nothing about it: %v", err)
		}
		g.assertNothingHolds(t, out, bootstrapCanary, g.cf.Tokens()[0].Value)
	})
	t.Run("token from the environment", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.env.LookupEnv = func(key string) (string, bool) {
			if key == "CLOUDFLARE_API_TOKEN" {
				return bootstrapCanary, true
			}
			return "", false
		}
		out := g.run(t, guidedAnswers(acceptedRest...), 0)
		g.assertNothingHolds(t, out, bootstrapCanary, g.cf.Tokens()[0].Value)
		if !strings.Contains(out, "Using the API token in CLOUDFLARE_API_TOKEN.") {
			t.Fatalf("no note about the environment token:\n%s", out)
		}
	})
}

func invalidKey() error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &smithy.GenericAPIError{Code: "InvalidAccessKeyId", Message: "The access key does not exist."}}
}

// failingOpener opens a store whose first listing fails as fail says, for as
// long as fail returns an error.
func failingOpener(fail func() error) func(config.Config) (storage.ObjectStore, error) {
	return func(config.Config) (storage.ObjectStore, error) {
		return &guidedStore{fail: fail}, nil
	}
}

// guidedStore is a bucket whose listing can be made to fail. Nothing else
// works on it: a test that gets past the listing has failed.
type guidedStore struct {
	storage.ObjectStore
	fail func() error
}

func (s *guidedStore) ListPage(context.Context, string, string, int32) (storage.ObjectPage, error) {
	return storage.ObjectPage{}, s.fail()
}

func TestGuidedR2TokenAndAccountFromTheEnvironment(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_API_TOKEN" {
			return "  " + bootstrapCanary + "\n", true
		}
		if key == "CLOUDFLARE_ACCOUNT_ID" {
			return strings.ToUpper(cloudflaretest.AccountID), true
		}
		return "", false
	}
	out := g.run(t, guidedAnswers(acceptedRest...), 0)
	if g.cf.Calls(cloudflaretest.RouteAccounts) != 0 || !strings.Contains(out, "Using the account ID in CLOUDFLARE_ACCOUNT_ID.") {
		t.Fatalf("accounts listed anyway:\n%s", out)
	}
	if strings.Contains(out, "Cloudflare API token (hidden") {
		t.Fatalf("asked for a token that the environment holds:\n%s", out)
	}
	if cfg := g.savedConfig(t); cfg.Storage.R2AccountID != cloudflaretest.AccountID {
		t.Fatalf("account %q", cfg.Storage.R2AccountID)
	}
}

func TestGuidedR2IgnoresAMalformedAccountIDInTheEnvironment(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.LookupEnv = func(key string) (string, bool) {
		if key == "CLOUDFLARE_ACCOUNT_ID" {
			return "not-an-account", true
		}
		return "", false
	}
	out := g.run(t, g.happy(), 0)
	if !strings.Contains(out, "CLOUDFLARE_ACCOUNT_ID isn't a Cloudflare account ID") || g.cf.Calls(cloudflaretest.RouteAccounts) != 1 {
		t.Fatalf("output:\n%s", out)
	}
}

// A blank token goes back to the storage menu, and nothing is created.
func TestGuidedR2BlankTokenReturnsToTheMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers("", "s3", "work", "2", ""), 0)
	if n := strings.Count(out, "Where should sessions be stored?"); n != 2 {
		t.Fatalf("menu shown %d times:\n%s", n, out)
	}
	if len(g.cf.Requests()) != 0 || len(g.apis) != 0 {
		t.Fatalf("Cloudflare was called: %v", routes(g.cf.Requests()))
	}
	if cfg := g.savedConfig(t); cfg.Storage.Provider != credentials.ProviderS3 {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// A token Cloudflare does not accept ends guided creation, saying so, and
// setup goes on with another option.
func TestGuidedR2RejectedTokenReturnsToTheMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers("some-other-token", "s3", "work", "2", ""), 0)
	if !strings.Contains(out, "Cloudflare didn't accept the API token") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 {
		t.Fatalf("output:\n%s", out)
	}
	if !g.apis[0].discarded {
		t.Fatal("the rejected token was not discarded")
	}
	g.assertNothingHolds(t, out, "some-other-token")
}

func TestGuidedR2AsksForTheAccountWhenItCannotBeFound(t *testing.T) {
	t.Parallel()
	t.Run("listing refused", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.cf.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "not allowed"})
		input := guidedAnswers(append(append([]string{}, askToken...), append([]string{"short", cloudflaretest.AccountID}, acceptedRest...)...)...)
		out := g.run(t, input, 0)
		for _, want := range []string{"Couldn't list your Cloudflare accounts", "isn't allowed to list accounts", "That isn't a Cloudflare account ID", "Cloudflare said: not allowed"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if g.savedConfig(t).Storage.R2AccountID != cloudflaretest.AccountID {
			t.Fatal("wrong account")
		}
	})
	t.Run("several accounts", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		other := "fedcba9876543210fedcba9876543210"
		g.cf.Accounts = append(g.cf.Accounts, cloudflaretest.Account{ID: other, Name: "Other account"})
		input := guidedAnswers(append(append([]string{}, askToken...), append([]string{other}, acceptedRest...)...)...)
		out := g.run(t, input, 0)
		if !strings.Contains(out, "more than one Cloudflare account") || !strings.Contains(out, "Other account") {
			t.Fatalf("accounts not listed:\n%s", out)
		}
		if g.savedConfig(t).Storage.R2AccountID != other {
			t.Fatal("wrong account")
		}
	})
	t.Run("no accounts", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.cf.Accounts = nil
		input := guidedAnswers(append(append([]string{}, askToken...), "0123456789abcdef0123456789abcdef", "", "n", "y", "", "stop")...)
		if out := g.run(t, input, 1); !strings.Contains(out, "Cloudflare doesn't know that account") {
			t.Fatalf("output:\n%s", out)
		}
	})
}

func TestGuidedR2ValidatesTheBucketName(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append([]string{}, askToken...), "Bad_Name", "ab", "-lead", "My-Archive-Bkt", "n", "y", "", "")...)
	out := g.run(t, input, 0)
	if strings.Count(out, "a bucket name") < 3 {
		t.Fatalf("names not refused:\n%s", out)
	}
	if got := g.savedConfig(t).Storage.Bucket; got != "my-archive-bkt" {
		t.Fatalf("bucket %q", got)
	}
	if _, ok := g.cf.Buckets["my-archive-bkt"]; !ok || len(g.cf.Buckets) != 1 {
		t.Fatalf("buckets %v", g.cf.Buckets)
	}
}

// A collision on the name setup generated is retried once with a new one.
func TestGuidedR2ReplacesACollidingDefaultName(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusConflict, Code: 10004, Message: "The bucket you tried to create already exists, and you own it.", Times: 1})
	out := g.run(t, g.happy(), 0)
	if !strings.Contains(out, "is taken; trying") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	var names []string
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateBucket {
			var body struct{ Name string }
			must(t, json.Unmarshal([]byte(r.Body), &body))
			names = append(names, body.Name)
		}
	}
	if names[0] == names[1] || !defaultBucketName.MatchString(names[1]) || g.savedConfig(t).Storage.Bucket != names[1] {
		t.Fatalf("names %v", names)
	}
}

// A collision on a name the person chose is asked about, never replaced.
func TestGuidedR2AsksAgainWhenAChosenNameIsTaken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Buckets["mine-bucket"] = ""
	input := guidedAnswers(append(append([]string{}, askToken...), "mine-bucket", "n", "y", "", "mine-two", "")...)
	out := g.run(t, input, 0)
	if !strings.Contains(out, "The name mine-bucket is taken in your Cloudflare account.") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	if got := g.savedConfig(t).Storage.Bucket; got != "mine-two" {
		t.Fatalf("bucket %q", got)
	}
}

// A jurisdiction reaches the header, the token's resource string, and the
// endpoint setup saves.
func TestGuidedR2JurisdictionAndLocationHint(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append([]string{}, askToken...), "eu-archive", "y", "mars", "EU", "nowhere", "weur", "y", "", "")...)
	out := g.run(t, input, 0)
	if !strings.Contains(out, "can't be changed later") {
		t.Fatalf("no warning about permanence:\n%s", out)
	}
	for _, r := range g.cf.Requests() {
		// Account-level calls carry no jurisdiction.
		accountLevel := slices.Contains([]cloudflaretest.Route{cloudflaretest.RouteAccounts, cloudflaretest.RoutePermissionGroups, cloudflaretest.RouteCreateToken, cloudflaretest.RouteDeleteToken}, r.Route)
		if !accountLevel && r.Jurisdiction != "eu" {
			t.Errorf("%s sent jurisdiction %q", r.Route, r.Jurisdiction)
		}
	}
	created := ""
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateBucket {
			created = r.Body
		}
	}
	if !strings.Contains(created, `"locationHint":"weur"`) {
		t.Fatalf("create body %s", created)
	}
	body, _ := json.Marshal(g.cf.Tokens()[0].Body)
	if !strings.Contains(string(body), "com.cloudflare.edge.r2.bucket."+cloudflaretest.AccountID+"_eu_eu-archive") {
		t.Fatalf("token request %s", body)
	}
	cfg := g.savedConfig(t)
	if cfg.Storage.R2Endpoint != "https://"+cloudflaretest.AccountID+".eu.r2.cloudflarestorage.com" || cfg.Storage.R2AccountID != "" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
	// The saved settings read back as a pasted jurisdiction URL does.
	loc, err := credentials.ParseR2Location(cfg.Storage.R2Endpoint)
	if err != nil || loc.Endpoint != cfg.Storage.R2Endpoint {
		t.Fatalf("location %+v (%v)", loc, err)
	}
}

// Declining the final question creates nothing.
func TestGuidedR2DecliningTheGoAheadCreatesNothing(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "n", "n", "s3", "work", "2", "")...), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || len(g.cf.Tokens()) != 0 {
		t.Fatalf("something was created:\n%s", out)
	}
}

// The retention rule is opt-in: without a yes, the bucket gets none, and its
// lifecycle is not touched.
func TestGuidedR2RetentionRuleIsOptional(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "", "", "")...), 0)
	if g.cf.Calls(cloudflaretest.RouteLifecycle) != 0 {
		t.Fatal("a lifecycle rule was set without a yes")
	}
}

func TestGuidedR2RetentionRuleFailureIsOnlyAWarning(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteLifecycle, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, g.happy(), 0)
	if !strings.Contains(out, "Couldn't set the bucket's deletion rule") || !strings.Contains(out, cloudflare.PermissionR2Write) {
		t.Fatalf("output:\n%s", out)
	}
	g.savedConfig(t)
}

// Public access that is on is a loud warning, not a failure and not a fix.
func TestGuidedR2WarnsWhenTheBucketIsPubliclyReachable(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.ManagedEnabled = true
	g.cf.CustomDomains = []string{"files.example.com"}
	out := g.run(t, g.happy(), 0)
	for _, want := range []string{"public r2.dev URL is ON", "anyone with the link can read", "files.example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "off (checked at setup)") || strings.Contains(out, "Custom domains: none") {
		t.Fatalf("claims access is off:\n%s", out)
	}
}

// When the reads are refused, setup says it did not check, and claims
// nothing.
func TestGuidedR2PublicAccessReadsRefused(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteManagedDomain, cloudflaretest.Failure{Status: http.StatusForbidden})
	g.cf.Fail(cloudflaretest.RouteCustomDomains, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, g.happy(), 0)
	for _, want := range []string{"Couldn't check whether the bucket's public r2.dev URL is on", "Couldn't check whether the bucket has custom domains"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(checked at setup)") {
		t.Fatalf("claims a check that did not happen:\n%s", out)
	}
	g.savedConfig(t)
}

// The failure menu after each step that can fail: what to fix, the bucket kept
// and reported, and no key made.
func TestGuidedR2StepFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		route   cloudflaretest.Route
		failure cloudflaretest.Failure
		want    []string
		bucket  bool
		mutate  func(*cloudflaretest.Server)
	}{
		{"bucket forbidden", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "no scope"},
			[]string{"Couldn't create the bucket", "needs the " + cloudflare.PermissionR2Write + " permission", "Cloudflare said: no scope"}, false, nil},
		{"bucket rate limited", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "600"},
			[]string{"limiting this token's requests; it asked for a wait of 600 seconds"}, false, nil},
		{"bucket server error", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusInternalServerError},
			[]string{"Cloudflare had a problem (HTTP 500)"}, false, nil},
		{"bucket account not found", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusNotFound},
			[]string{"Cloudflare doesn't know that account or bucket"}, false, nil},
		{"permission group lookup forbidden", cloudflaretest.RoutePermissionGroups, cloudflaretest.Failure{Status: http.StatusForbidden},
			[]string{"Couldn't look up the permission", cloudflare.PermissionTokensWrite, "subset of their own permissions"}, true, nil},
		{"permission group missing", "", cloudflaretest.Failure{},
			[]string{"Couldn't find the permission for the bucket's key", "is listed"}, true, func(s *cloudflaretest.Server) { s.Groups = nil }},
		{"permission group not selectable", "", cloudflaretest.Failure{},
			[]string{"may not grant it"}, true, func(s *cloudflaretest.Server) {
				s.Groups = []cloudflaretest.Group{{ID: "aaaa0000000000000000000000000002", Name: cloudflare.PermissionBucketItemWrite}}
			}},
		{"token forbidden", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "Unauthorized to access requested resource"},
			[]string{"Couldn't create the key", cloudflare.PermissionTokensWrite, "subset of their own permissions", "administrator"}, true, nil},
		{"token conflict", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusConflict},
			[]string{"Couldn't create the key", "already exists"}, true, nil},
		{"token server error", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusBadGateway},
			[]string{"Cloudflare had a problem (HTTP 502)", "If Cloudflare did create it, revoke the token named"}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGuidedR2Fixture(t)
			if tc.route != "" {
				g.cf.Fail(tc.route, tc.failure)
			}
			if tc.mutate != nil {
				tc.mutate(g.cf)
			}
			input := guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...)
			out := g.run(t, input, 1)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if left := strings.Contains(out, "was created and is empty"); left != tc.bucket {
				t.Errorf("bucket reported left behind = %v, want %v:\n%s", left, tc.bucket, out)
			}
			if len(g.cf.Live()) != 0 || len(g.keychain.items) != 0 {
				t.Errorf("a key was made: %d tokens, %d stored", len(g.cf.Live()), len(g.keychain.items))
			}
			g.notSaved(t)
			for _, api := range g.apis {
				if !api.discarded {
					t.Error("the bootstrap token was not discarded")
				}
			}
			g.assertNothingHolds(t, out, bootstrapCanary)
		})
	}
}

// The token's name is printed before it is created, so a crash leaves a
// recognizable orphan: here creation fails, and the name printed is the name
// that was sent.
func TestGuidedR2PrintsTheTokenNameBeforeCreatingIt(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...), 1)
	var sent struct{ Name string }
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateToken {
			must(t, json.Unmarshal([]byte(r.Body), &sent))
		}
	}
	if sent.Name == "" || !strings.Contains(out, `an API token named "`+sent.Name+`"`) {
		t.Fatalf("name sent %q; output:\n%s", sent.Name, out)
	}
	if strings.Index(out, "revoke that token in the dashboard") > strings.Index(out, "Couldn't create the key") {
		t.Fatalf("the recovery text came after the failure:\n%s", out)
	}
}

// A retry after a failed token step reuses the bucket, and the permission
// group looked up once.
func TestGuidedR2RetryReusesTheBucket(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "retry", "")...), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 1 || g.cf.Calls(cloudflaretest.RoutePermissionGroups) != 1 || g.cf.Calls(cloudflaretest.RouteCreateToken) != 2 {
		t.Fatalf("calls %v", routes(g.cf.Requests()))
	}
	if !strings.Contains(out, "Try again with the same bucket") || len(g.cf.Live()) != 1 {
		t.Fatalf("output:\n%s", out)
	}
	g.savedConfig(t)
}

// After a failed bucket step, a retry creates it.
func TestGuidedR2RetryAfterAFailedBucketStep(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusInternalServerError, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "retry", "")...), 0)
	if strings.Contains(out, "with the same bucket") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	g.savedConfig(t)
}

// Choosing another option after a failure ends guided creation, reports the
// bucket left behind, and setup carries on with the other option.
func TestGuidedR2FailureThenAnotherOption(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "other", "s3", "work", "2", "")...), 0)
	if !strings.Contains(out, "was created and is empty. It stays in your Cloudflare account") {
		t.Fatalf("bucket not reported:\n%s", out)
	}
	if cfg := g.savedConfig(t); cfg.Storage.Provider != credentials.ProviderS3 {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// A key that never passes the storage check is revoked, the check is
// repeated while Cloudflare may still be starting to honor the key, and
// nothing is stored.
func TestGuidedR2RevokesAKeyThatFailsVerification(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var listings atomic.Int32
	g.env.OpenStore = failingOpener(func() error { listings.Add(1); return invalidKey() })
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...), 1)
	if n := listings.Load(); int(n) != r2VerifyAttempts || g.pauses != r2VerifyAttempts-1 {
		t.Fatalf("%d checks and %d pauses, want %d and %d", n, g.pauses, r2VerifyAttempts, r2VerifyAttempts-1)
	}
	for _, want := range []string{"The new key didn't pass the storage check.", "Can't sign in to Cloudflare R2.", "Revoked the key that failed the check.", "Waiting for Cloudflare to start accepting the new key"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 || g.cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
		t.Fatalf("tokens %d, live %d", len(g.cf.Tokens()), len(g.cf.Live()))
	}
	if len(g.keychain.items) != 0 {
		t.Fatalf("a failed key was stored: %v", g.keychain.items)
	}
	// Setup ends without the lifecycle rule or the public-access reads: the
	// bucket is unused.
	if g.cf.Calls(cloudflaretest.RouteLifecycle) != 0 || g.cf.Calls(cloudflaretest.RouteManagedDomain) != 0 {
		t.Fatalf("calls %v", routes(g.cf.Requests()))
	}
	g.notSaved(t)
}

// Trying again after a failed check makes a fresh key for the same bucket.
func TestGuidedR2RetryAfterFailedVerification(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var opened atomic.Int32
	g.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if int(opened.Add(1)) <= r2VerifyAttempts {
			return &guidedStore{fail: invalidKey}, nil
		}
		return g.bucket, nil
	}
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "retry", "")...), 0)
	tokens := g.cf.Tokens()
	if len(tokens) != 2 || len(g.cf.Live()) != 1 || g.cf.Live()[0].ID != tokens[1].ID || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 1 {
		t.Fatalf("tokens %+v, calls %v\n%s", tokens, routes(g.cf.Requests()), out)
	}
	if tokens[0].Name == tokens[1].Name {
		t.Fatalf("the retry reused a token name: %q", tokens[0].Name)
	}
	cfg := g.savedConfig(t)
	stored, err := g.keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || stored.AccessKeyID != tokens[1].ID {
		t.Fatalf("stored %+v (%v), want the second token", stored, err)
	}
}

// A refused revoke names the token to remove by hand.
func TestGuidedR2FailedRevokeNamesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.OpenStore = failingOpener(invalidKey)
	g.cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...), 1)
	name := g.cf.Tokens()[0].Name
	if !strings.Contains(out, "Couldn't revoke the key that failed the check.") || !strings.Contains(out, `Revoke the API token named "`+name+`"`) {
		t.Fatalf("output:\n%s", out)
	}
}

// Only failures that can be Cloudflare catching up are waited out.
func TestGuidedR2DoesNotWaitOutOtherFailures(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var listings atomic.Int32
	g.env.OpenStore = failingOpener(func() error { listings.Add(1); return errors.New("connection reset") })
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "y", "", "stop")...), 1)
	if listings.Load() != 1 || g.pauses != 0 {
		t.Fatalf("%d checks, %d pauses", listings.Load(), g.pauses)
	}
	if !strings.Contains(out, "Revoked the key that failed the check.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestGuidedR2RateLimitIsWaitedOutThenSucceeds(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "5", Times: 2})
	out := g.run(t, g.happy(), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 3 || strings.Contains(out, "limiting this token") {
		t.Fatalf("output:\n%s", out)
	}
}

// Ending the flow any way but success still discards the token.
func TestGuidedR2DiscardsTheTokenWhenTheFlowEnds(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.run(t, g.happy(), 0)
	if len(g.apis) != 1 || !g.apis[0].discarded {
		t.Fatalf("clients %d, discarded %v", len(g.apis), g.apis[0].discarded)
	}
	if _, err := g.apis[0].Accounts(context.Background()); err == nil {
		t.Fatal("the discarded client still works")
	}
}

// The menu offers the guided choice, and its number works.
func TestGuidedR2IsInTheStorageMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := strings.Join(append([]string{"", "3"}, append(append([]string{}, askToken...), acceptedRest...)...), "\n") + "\n"
	out := g.run(t, input, 0)
	if !strings.Contains(out, "3) Create a new R2 bucket for me") {
		t.Fatalf("menu:\n%s", out)
	}
	g.savedConfig(t)
}

// setup --yes never reaches Cloudflare: the guided path is interactive only.
func TestGuidedR2IsNotPartOfSetupYes(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.Cloudflare = func(string) cloudflare.API {
		t.Error("setup --yes reached Cloudflare")
		return nil
	}
	var out bytes.Buffer
	args := []string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", filepath.Join(g.userHome, "src", "web-app")}
	if code := Run(args, strings.NewReader(""), &out, &out, g.env); code != 0 {
		t.Fatalf("setup --yes exit %d\n%s", code, &out)
	}
}

func TestExplainCloudflare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{"unauthorized", &cloudflare.Error{Status: 401}, []string{"didn't accept the API token"}},
		{"forbidden with step text", &cloudflare.Error{Status: 403}, []string{"STEP TEXT"}},
		{"not found", &cloudflare.Error{Status: 404}, []string{"doesn't know that account or bucket"}},
		{"conflict", &cloudflare.Error{Status: 409}, []string{"already exists"}},
		{"rate limited without a wait", &cloudflare.Error{Status: 429}, []string{"limiting this token's requests. Try again in a few minutes."}},
		{"rate limited", &cloudflare.Error{Status: 429, RetryAfter: 90 * time.Second}, []string{"asked for a wait of 90 seconds"}},
		{"server", &cloudflare.Error{Status: 503}, []string{"HTTP 503"}},
		{"other", &cloudflare.Error{Status: 418}, []string{"refused the request (HTTP 418)"}},
		{"no response", &cloudflare.Error{Err: errors.New("dial tcp: refused")}, []string{"Couldn't reach Cloudflare", "dial tcp: refused"}},
		{"message", &cloudflare.Error{Status: 500, Messages: []string{"boom"}}, []string{"Cloudflare said: boom"}},
		{"long message", &cloudflare.Error{Status: 500, Messages: []string{strings.Repeat("x", 500)}}, []string{strings.Repeat("x", 200) + "…"}},
		{"another error", errors.New("plain"), []string{"couldn't complete the request: plain"}},
	}
	for _, tc := range cases {
		text := explainCloudflare(tc.err, "STEP TEXT")
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: %q lacks %q", tc.name, text, want)
			}
		}
		if strings.Contains(text, strings.Repeat("x", 201)) {
			t.Errorf("%s: message not cut", tc.name)
		}
	}
	if text := explainCloudflare(&cloudflare.Error{Status: 403}, ""); !strings.Contains(text, "doesn't have the permission") {
		t.Errorf("empty 403 text: %q", text)
	}
}

func TestParseR2AccountID(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0123456789abcdef0123456789abcdef", " 0123456789ABCDEF0123456789ABCDEF ", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"} {
		if got, err := parseR2AccountID(ok); err != nil || got != "0123456789abcdef0123456789abcdef" {
			t.Errorf("%q: %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "0123456789abcdef0123456789abcdeg", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/bucket", "https://example.com"} {
		if got, err := parseR2AccountID(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}
