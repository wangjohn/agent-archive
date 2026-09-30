package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Guided R2 creation: setup makes the bucket and a key that reaches only that
// bucket, from one Cloudflare API token the person pastes. The design, and
// what it leaves unconfirmed, is Part 2a of
// dev/proposals/portable-handoff-and-onboarding.md.
//
// The pasted "bootstrap" token is the only value here that can do more than
// read and write one bucket. It exists only in local variables of
// createR2Bucket and in the cloudflare.Client made from it; it is never put
// in the setup draft, the journal, the configuration, the Keychain, the
// environment of any program setup starts, a log, or an error. The key that
// is stored is the derived, bucket-scoped one, and it goes through the same
// staging (advanceSetupDraft) as a key pasted by hand.
//
// The flow verifies that key against the new bucket itself, with the bucket
// creation still in reach, so a key that does not work can be revoked at
// once. The ordinary storage check that follows setup's storage questions
// then runs on the stored key as it does for any R2 key.

// guidedR2Choice is the storage menu's key for creating an R2 bucket.
const guidedR2Choice = "r2-create"

func guidedR2Option() option {
	return option{guidedR2Choice, "Create a new R2 bucket for me"}
}

// errChooseStorageAgain ends guided creation without a bucket: the person
// asked for another storage option, or gave nothing to create one with.
var errChooseStorageAgain = errors.New("choose another storage option")

// r2VerifyAttempts and r2VerifyPause bound how long setup waits for
// Cloudflare to start honoring a key it just created. How long that takes is
// not documented (see "Live acceptance" in dev/contributing/testing.md).
const (
	r2VerifyAttempts = 5
	r2VerifyPause    = 3 * time.Second
)

// r2Creator is one run of guided creation.
type r2Creator struct {
	p       *prompter
	env     Env
	api     cloudflare.API
	account string
	bucket  cloudflare.BucketSpec
	// defaultName is whether the bucket name is the generated one, which
	// may be replaced by a new one if it collides.
	defaultName bool
	// retention, when positive, is the age after which the bucket's own
	// lifecycle rule deletes objects.
	retention     time.Duration
	bucketCreated bool
	groupID       string
}

// createR2Bucket runs guided creation and returns the storage settings and
// the derived key for setup to stage and check, as promptStorage does for a
// key typed in.
func createR2Bucket(p *prompter, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	var none credentials.R2Credentials
	printR2BootstrapInstructions(p)
	token, err := askBootstrapToken(p, env)
	if err != nil {
		return credentials.Config{}, none, false, err
	}
	c := &r2Creator{p: p, env: env, api: env.cloudflareAPI(token)}
	// However this ends, the token is dropped: the key that is kept was
	// made from it, and nothing else needs it.
	defer c.api.Discard()
	if err = c.chooseAccount(); err != nil {
		return credentials.Config{}, none, false, err
	}
	if err = c.askBucket(); err != nil {
		return credentials.Config{}, none, false, err
	}
	key, err := c.createUntilVerified()
	if err != nil {
		return credentials.Config{}, none, false, err
	}
	c.finish()
	terminal.Println(p.out, "The Cloudflare API token you pasted is not saved anywhere. You can delete it in the dashboard; the archive doesn't need it again.")
	cfg := c.storageConfig()
	return cfg, key, true, nil
}

// storageConfig is what a pasted account ID or bucket URL would have made:
// the account ID with its endpoint, or, in a jurisdiction, only the
// jurisdiction's endpoint, as credentials.ParseR2Location reads it.
func (c *r2Creator) storageConfig() credentials.Config {
	endpoint, _ := credentials.R2Endpoint(cloudflare.Endpoint(c.account, c.bucket.Jurisdiction), "")
	accountID := c.account
	if c.bucket.Jurisdiction != "" {
		accountID = ""
	}
	return credentials.Config{Provider: credentials.ProviderR2, Bucket: c.bucket.Name, R2Endpoint: endpoint, R2AccountID: accountID, Prefix: defaultPrefix}
}

func printR2BootstrapInstructions(p *prompter) {
	out := p.out
	terminal.Println(out, "Setup can create a private Cloudflare R2 bucket, and a key that reaches only that bucket.")
	terminal.Println(out, "It needs one Cloudflare API token to do that. You create the token; setup uses it now and then discards it. It is never saved.")
	terminal.Println(out, "")
	terminal.Println(out, "In the Cloudflare dashboard, go to Manage account > Account API tokens > Create Token, and give the token these permissions on your account:")
	terminal.Println(out, "  - "+cloudflare.PermissionR2Write)
	terminal.Println(out, "  - "+cloudflare.PermissionTokensWrite)
	terminal.Println(out, "Cloudflare's steps: "+cloudflare.TokenDocsURL)
	terminal.Println(out, "")
}

// askBootstrapToken reads the bootstrap token from CLOUDFLARE_API_TOKEN, as
// Cloudflare's own tools do, or asks for it without echoing it. An empty
// answer goes back to the storage menu.
func askBootstrapToken(p *prompter, env Env) (string, error) {
	if value, ok := env.lookupEnv("CLOUDFLARE_API_TOKEN"); ok && strings.TrimSpace(value) != "" {
		terminal.Println(p.out, "Using the API token in CLOUDFLARE_API_TOKEN.")
		return strings.TrimSpace(value), nil
	}
	token, err := p.secret("Cloudflare API token (hidden; Enter to choose another option): ")
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errChooseStorageAgain
	}
	return token, nil
}

// chooseAccount settles which Cloudflare account owns the bucket: the one
// in CLOUDFLARE_ACCOUNT_ID, else the token's only account, else the one the
// person names. Whether the token may list accounts is not documented, so a
// refusal is a reason to ask, not a failure; only a token Cloudflare does
// not accept ends the flow.
func (c *r2Creator) chooseAccount() error {
	if value, ok := c.env.lookupEnv("CLOUDFLARE_ACCOUNT_ID"); ok && strings.TrimSpace(value) != "" {
		if account, err := parseR2AccountID(value); err == nil {
			terminal.Println(c.p.out, "Using the account ID in CLOUDFLARE_ACCOUNT_ID.")
			c.account = account
			return nil
		}
		c.p.warn("CLOUDFLARE_ACCOUNT_ID isn't a Cloudflare account ID (32 characters, 0-9 and a-f); ignoring it.")
	}
	accounts, err := c.api.Accounts(context.Background())
	var apiErr *cloudflare.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Unauthorized():
		terminal.Println(c.p.out, c.p.style.failMark()+" "+explainCloudflare(err, ""))
		return errChooseStorageAgain
	case err != nil:
		terminal.Println(c.p.out, "Couldn't list your Cloudflare accounts, so setup needs the account ID. "+explainCloudflare(err, "The token isn't allowed to list accounts."))
	case len(accounts) == 1:
		if account, e := parseR2AccountID(accounts[0].ID); e == nil {
			terminal.Printf(c.p.out, "Cloudflare account: %s (%s)\n", accounts[0].Name, account)
			c.account = account
			return nil
		}
	case len(accounts) > 1:
		terminal.Println(c.p.out, "The token can see more than one Cloudflare account:")
		for _, a := range accounts {
			terminal.Printf(c.p.out, "  %s  %s\n", a.ID, a.Name)
		}
	}
	terminal.Println(c.p.out, "Find the Account ID in the Cloudflare dashboard > Storage & databases > R2 > Overview.")
	for {
		answer, err := c.p.required("Cloudflare account ID", "")
		if err != nil {
			return err
		}
		account, e := parseR2AccountID(answer)
		if e == nil {
			c.account = account
			return nil
		}
		terminal.Printf(c.p.out, "That isn't a Cloudflare account ID (%v).\n", e)
	}
}

// parseR2AccountID reads a 32-character account ID.
func parseR2AccountID(input string) (string, error) {
	loc, err := credentials.ParseR2Location(input)
	if err != nil {
		return "", err
	}
	if loc.AccountID == "" || loc.Bucket != "" {
		return "", errors.New("an R2 account ID is 32 characters, 0-9 and a-f")
	}
	return loc.AccountID, nil
}

// askBucket asks for the bucket's name, its optional location, and whether
// its own lifecycle rule should enforce retention, then for a go-ahead.
func (c *r2Creator) askBucket() error {
	p := c.p
	suggested, err := randomBucketName()
	if err != nil {
		return err
	}
	name, err := c.askBucketName("Bucket name", suggested)
	if err != nil {
		return err
	}
	c.bucket.Name, c.defaultName = name, name == suggested
	if err = c.askLocation(); err != nil {
		return err
	}
	if days := p.storageRetentionDays; days > 0 {
		terminal.Printf(p.out, "You keep sessions for %d days; the archive deletes older ones itself. A rule on the bucket can enforce that at Cloudflare too, even while this Mac is off.\n", days)
		terminal.Println(p.out, "The rule doesn't follow later changes to your retention setting.")
		yes, e := p.yesNo(fmt.Sprintf("Also have Cloudflare delete objects after %d days?", days), false)
		if e != nil {
			return e
		}
		if yes {
			c.retention = time.Duration(days) * 24 * time.Hour
		}
	}
	ok, err := p.yesNo("Create the bucket and its key now?", true)
	if err != nil {
		return err
	}
	if !ok {
		return errChooseStorageAgain
	}
	return nil
}

func (c *r2Creator) askBucketName(label, def string) (string, error) {
	for {
		name, err := c.p.required(label, def)
		if err != nil {
			return "", err
		}
		name = strings.ToLower(name)
		if e := cloudflare.ValidateBucketName(name); e != nil {
			terminal.Println(c.p.out, e.Error()+".")
			continue
		}
		return name, nil
	}
}

func (c *r2Creator) askLocation() error {
	p := c.p
	change, err := p.yesNo("Choose where Cloudflare stores the bucket? Most people skip this.", false)
	if err != nil || !change {
		return err
	}
	for {
		answer, e := p.withDefault("Jurisdiction ("+strings.Join(cloudflare.Jurisdictions, ", ")+"; Enter for none)", "")
		if e != nil {
			return e
		}
		if answer == "" || cloudflare.ValidJurisdiction(strings.ToLower(answer)) {
			c.bucket.Jurisdiction = strings.ToLower(answer)
			break
		}
		terminal.Println(p.out, "Choose one of "+strings.Join(cloudflare.Jurisdictions, ", ")+", or press Enter for none.")
	}
	if c.bucket.Jurisdiction != "" {
		terminal.Println(p.out, "A jurisdiction can't be changed later, and the bucket is reached only at "+cloudflare.Endpoint(c.account, c.bucket.Jurisdiction)+".")
	}
	for {
		answer, e := p.withDefault("Location hint ("+strings.Join(cloudflare.LocationHints, ", ")+"; Enter for automatic)", "")
		if e != nil {
			return e
		}
		if answer == "" || cloudflare.ValidLocationHint(strings.ToLower(answer)) {
			c.bucket.LocationHint = strings.ToLower(answer)
			return nil
		}
		terminal.Println(p.out, "Choose one of "+strings.Join(cloudflare.LocationHints, ", ")+", or press Enter for automatic.")
	}
}

// randomBucketName is agent-archive-<6 random hex>.
func randomBucketName() (string, error) {
	suffix, err := randomHex(3)
	if err != nil {
		return "", err
	}
	return "agent-archive-" + suffix, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random name: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// createUntilVerified creates the bucket, mints the key, and checks it, and
// after a failure offers to try again (reusing the bucket), to pick another
// storage option, or to stop. A key that failed its check is revoked before
// any of those.
func (c *r2Creator) createUntilVerified() (credentials.R2Credentials, error) {
	for {
		key, err := c.attempt()
		if err == nil {
			return key, nil
		}
		var choices []option
		retry := "Try again"
		if c.bucketCreated {
			retry = "Try again with the same bucket (" + c.bucket.Name + ")"
		}
		choices = append(choices, option{"retry", retry}, option{"other", "Choose another storage option"}, option{"stop", "Stop setup"})
		choice, e := c.p.menu("What next?", "retry", choices...)
		if e != nil {
			return credentials.R2Credentials{}, e
		}
		if choice == "retry" {
			continue
		}
		c.reportBucketLeftBehind()
		if choice == "other" {
			return credentials.R2Credentials{}, errChooseStorageAgain
		}
		return credentials.R2Credentials{}, errors.New("guided bucket creation stopped")
	}
}

// reportBucketLeftBehind says so when a bucket was created and will not be
// used: it is empty, and it stays in the person's Cloudflare account.
func (c *r2Creator) reportBucketLeftBehind() {
	if c.bucketCreated {
		terminal.Printf(c.p.out, "The bucket %s was created and is empty. It stays in your Cloudflare account; delete it in the dashboard (R2 > %s > Settings) if you don't want it.\n", c.bucket.Name, c.bucket.Name)
	}
}

// attempt is one pass through creating the bucket (once), the key, and its
// check. It prints why it failed and returns the failure.
func (c *r2Creator) attempt() (credentials.R2Credentials, error) {
	if !c.bucketCreated {
		if err := c.createBucket(); err != nil {
			return credentials.R2Credentials{}, err
		}
	}
	if c.groupID == "" {
		id, err := c.lookUpPermissionGroup()
		if err != nil {
			return credentials.R2Credentials{}, err
		}
		c.groupID = id
	}
	token, name, key, err := c.mintKey()
	if err != nil {
		return credentials.R2Credentials{}, err
	}
	if err = c.checkKey(key); err != nil {
		c.revoke(token, name)
		return credentials.R2Credentials{}, err
	}
	return key, nil
}

// createBucket makes the bucket. A name that is taken is replaced once if
// setup chose it, and otherwise asked for again.
func (c *r2Creator) createBucket() error {
	p := c.p
	replaced := false
	for {
		err := c.api.CreateBucket(context.Background(), c.account, c.bucket)
		if err == nil {
			c.bucketCreated = true
			terminal.Println(p.out, p.style.okMark()+" Created bucket "+c.bucket.Name+".")
			return nil
		}
		var apiErr *cloudflare.Error
		if !errors.As(err, &apiErr) || !apiErr.AlreadyExists() {
			terminal.Println(p.out, p.style.failMark()+" Couldn't create the bucket. "+explainCloudflare(err, "The token needs the "+cloudflare.PermissionR2Write+" permission to create buckets; add it to the token in the dashboard."))
			return err
		}
		if c.defaultName && !replaced {
			replaced = true
			name, e := randomBucketName()
			if e != nil {
				return e
			}
			terminal.Printf(p.out, "The name %s is taken; trying %s.\n", c.bucket.Name, name)
			c.bucket.Name = name
			continue
		}
		terminal.Printf(p.out, "The name %s is taken in your Cloudflare account.\n", c.bucket.Name)
		name, e := c.askBucketName("Another bucket name", "")
		if e != nil {
			return e
		}
		c.bucket.Name, c.defaultName = name, false
	}
}

// lookUpPermissionGroup finds the ID of the bucket-item-write group. It is
// asked for every time and never written down: Cloudflare documents the ID,
// not the name, as the stable key.
func (c *r2Creator) lookUpPermissionGroup() (string, error) {
	p := c.p
	groups, err := c.api.PermissionGroups(context.Background(), c.account, cloudflare.PermissionBucketItemWrite)
	if err == nil {
		var id string
		if id, err = cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite); err == nil {
			return id, nil
		}
		terminal.Println(p.out, p.style.failMark()+" Couldn't find the permission for the bucket's key: "+err.Error()+".")
		terminal.Println(p.out, "The token needs the "+cloudflare.PermissionTokensWrite+" permission, and its owner must be allowed to grant "+cloudflare.PermissionBucketItemWrite+".")
		return "", err
	}
	terminal.Println(p.out, p.style.failMark()+" Couldn't look up the permission for the bucket's key. "+explainCloudflare(err, tokenWriteHint))
	return "", err
}

// tokenWriteHint is what a refused token step needs. Cloudflare lets a member
// grant only permissions the member has, so a token made by a member who is
// not an administrator can be refused here even with the permission.
const tokenWriteHint = "The token needs the " + cloudflare.PermissionTokensWrite + " permission. Account members can grant a token only a subset of their own permissions, so an account administrator may need to create it."

// mintKey creates the bucket-scoped token and derives its S3 key. The
// token's name is printed first: a crash between its creation and the key
// being saved loses the token's value, which Cloudflare shows only once, and
// the name is how the person recognizes what to revoke.
func (c *r2Creator) mintKey() (token cloudflare.Token, name string, key credentials.R2Credentials, err error) {
	p := c.p
	suffix, err := randomHex(3)
	if err != nil {
		return token, "", key, err
	}
	name = "agent-archive " + c.bucket.Name + " " + suffix
	terminal.Println(p.out, "Creating the bucket's key, an API token named \""+name+"\".")
	terminal.Println(p.out, "If setup stops before it finishes, revoke that token in the dashboard (Manage account > Account API tokens).")
	spec := cloudflare.TokenSpec{
		Name: name,
		Policies: []cloudflare.Policy{{
			PermissionGroupIDs: []string{c.groupID},
			Resources:          map[string]string{cloudflare.BucketResource(c.account, c.bucket.BucketRef): "*"},
		}},
	}
	token, err = c.api.CreateToken(context.Background(), c.account, spec)
	if err != nil {
		terminal.Println(p.out, p.style.failMark()+" Couldn't create the key. "+explainCloudflare(err, tokenWriteHint))
		var apiErr *cloudflare.Error
		if !errors.As(err, &apiErr) || apiErr.ServerError() || apiErr.Status == 0 {
			// The answer may have been lost after Cloudflare acted.
			terminal.Println(p.out, "If Cloudflare did create it, revoke the token named \""+name+"\" in the dashboard.")
		}
		return token, name, key, err
	}
	derived := cloudflare.DeriveS3Credentials(token)
	key = credentials.R2Credentials{AccessKeyID: derived.AccessKeyID, SecretAccessKey: derived.SecretAccessKey}
	terminal.Println(p.out, p.style.okMark()+" Created a key for "+c.bucket.Name+" only.")
	return token, name, key, nil
}

// checkKey runs setup's storage check (a listing, then the write, read,
// list, delete round trip) on the new key, waiting a little while Cloudflare
// starts honoring it, and prints why it failed.
func (c *r2Creator) checkKey(key credentials.R2Credentials) error {
	p := c.p
	cfg := c.storageConfig()
	var err error
	for attempt := 1; attempt <= r2VerifyAttempts; attempt++ {
		if err = c.env.verifyR2Key(cfg, key); err == nil {
			terminal.Println(p.out, p.style.okMark()+" The new key can read and write "+c.bucket.Name+".")
			return nil
		}
		if cause := storage.Diagnose(err).Cause; cause != storage.CauseNoCredentials && cause != storage.CauseAccessDenied {
			break
		}
		if attempt == 1 {
			terminal.Println(p.out, "Waiting for Cloudflare to start accepting the new key…")
		}
		if attempt < r2VerifyAttempts {
			c.env.pause(r2VerifyPause)
		}
	}
	d := storageDiagnosis(cfg, err)
	terminal.Println(p.out, p.style.failMark()+" The new key didn't pass the storage check.")
	terminal.Println(p.out, "  "+storageFailureHeadline(cfg, d))
	terminal.Println(p.out, "  "+d.Explanation)
	return err
}

// revoke deletes a token whose key failed its check, so no working
// credential is left behind. If Cloudflare refuses, the person is told which
// token to remove.
func (c *r2Creator) revoke(token cloudflare.Token, name string) {
	p := c.p
	err := c.api.DeleteToken(context.Background(), c.account, token.ID)
	if err == nil {
		terminal.Println(p.out, p.style.okMark()+" Revoked the key that failed the check.")
		return
	}
	p.warn("Couldn't revoke the key that failed the check. "+explainCloudflare(err, "The token needs the "+cloudflare.PermissionTokensWrite+" permission to revoke tokens."),
		"Revoke the API token named \""+name+"\" in the dashboard (Manage account > Account API tokens).")
}

// finish does the optional extras once the key works: the retention rule,
// and reading the bucket's public-access settings. Neither can fail setup.
func (c *r2Creator) finish() {
	p := c.p
	if c.retention > 0 {
		// The rule replaces all the bucket's lifecycle rules. That is safe
		// only because this run just created the bucket, so it is set here
		// and nowhere else.
		err := c.api.ExpireObjectsAfter(context.Background(), c.account, c.bucket.BucketRef, c.retention)
		if err != nil {
			p.warn("Couldn't set the bucket's deletion rule; sessions are still deleted by the archive on its own schedule. "+explainCloudflare(err, "The token needs the "+cloudflare.PermissionR2Write+" permission to set it."), "You can add a lifecycle rule in the dashboard (R2 > "+c.bucket.Name+" > Settings).")
		} else {
			terminal.Printf(p.out, "%s The bucket deletes objects after %d days.\n", p.style.okMark(), int(c.retention/(24*time.Hour)))
		}
	}
	c.reportPublicAccess()
}

// reportPublicAccess says what the bootstrap token could see of the bucket's
// public access, and only that: the r2.dev URL and the custom domains, read
// once, now. It is not stored and not repeated later, and it says nothing of
// access setup cannot read. A fresh bucket is private by default, so this
// confirms that; it fixes nothing.
//
// Which token permissions these two reads need is not documented; a refusal
// is reported as "not checked".
func (c *r2Creator) reportPublicAccess() {
	p := c.p
	ctx := context.Background()
	domain, managedErr := c.api.ManagedDomain(ctx, c.account, c.bucket.BucketRef)
	custom, customErr := c.api.CustomDomains(ctx, c.account, c.bucket.BucketRef)
	var enabled []string
	for _, d := range custom {
		if d.Enabled {
			enabled = append(enabled, d.Domain)
		}
	}
	switch {
	case managedErr == nil && domain.Enabled:
		p.warn(p.style.fail("The bucket's public r2.dev URL is ON: anyone with the link can read what is stored."),
			"Turn it off before archiving: Cloudflare dashboard > R2 > "+c.bucket.Name+" > Settings > Public access.")
	case managedErr == nil:
		terminal.Println(p.out, p.style.okMark()+" r2.dev public access: off (checked at setup).")
	default:
		p.warn("Couldn't check whether the bucket's public r2.dev URL is on. "+explainCloudflare(managedErr, "The token may need the Workers R2 Storage Read permission to read it."),
			"Check it in the dashboard: R2 > "+c.bucket.Name+" > Settings > Public access.")
	}
	switch {
	case customErr == nil && len(enabled) > 0:
		p.warn(p.style.fail("The bucket serves these custom domains publicly: "+strings.Join(enabled, ", ")+"."), "Remove them if the archive should stay private.")
	case customErr == nil:
		terminal.Println(p.out, p.style.okMark()+" Custom domains: none (checked at setup).")
	default:
		p.warn("Couldn't check whether the bucket has custom domains. " + explainCloudflare(customErr, "The token may need the Workers R2 Storage Read permission to read them."))
	}
	terminal.Println(p.out, p.style.dim("  These were read once, just now; setup doesn't check again."))
}

// explainCloudflare says, in one or two sentences, what a failed Cloudflare
// call needs from the person. forbidden is what to say for a 403, which names
// the permission that step uses. Cloudflare's own message follows: it never
// holds the token (the client removes it).
func explainCloudflare(err error, forbidden string) string {
	var apiErr *cloudflare.Error
	if !errors.As(err, &apiErr) {
		return "Setup couldn't complete the request: " + err.Error() + "."
	}
	var text string
	switch {
	case apiErr.Status == 0:
		text = "Couldn't reach Cloudflare. Check your connection, then try again."
	case apiErr.Unauthorized():
		text = "Cloudflare didn't accept the API token. Check that it was copied whole and hasn't expired or been deleted."
	case apiErr.Forbidden():
		text = forbidden
		if text == "" {
			text = "The token doesn't have the permission this step needs."
		}
	case apiErr.NotFound():
		text = "Cloudflare doesn't know that account or bucket. Check the account ID, and that the token belongs to that account."
	case apiErr.AlreadyExists():
		text = "Cloudflare says it already exists."
	case apiErr.RateLimited():
		text = "Cloudflare is limiting this token's requests"
		if apiErr.RetryAfter > 0 {
			text += fmt.Sprintf("; it asked for a wait of %d seconds", int(apiErr.RetryAfter/time.Second))
		}
		text += ". Try again in a few minutes."
	case apiErr.ServerError():
		text = fmt.Sprintf("Cloudflare had a problem (HTTP %d). Try again in a few minutes.", apiErr.Status)
	default:
		text = fmt.Sprintf("Cloudflare refused the request (HTTP %d).", apiErr.Status)
	}
	if len(apiErr.Messages) > 0 {
		message := apiErr.Messages[0]
		if len(message) > 200 {
			message = message[:200] + "…"
		}
		text += " Cloudflare said: " + message
	}
	if apiErr.Status == 0 && apiErr.Err != nil {
		text += " (" + apiErr.Err.Error() + ")"
	}
	return text
}

// cloudflareAPI is the API client for a bootstrap token: Env.Cloudflare, or the
// real API.
func (e Env) cloudflareAPI(token string) cloudflare.API {
	if e.Cloudflare != nil {
		return e.Cloudflare(token)
	}
	return openCloudflare(token)
}

// openCloudflare is Env.Cloudflare's default. The package's tests replace it
// with one that stops the test, so a test that leaves Env.Cloudflare unset
// never reaches Cloudflare.
var openCloudflare = func(token string) cloudflare.API {
	return cloudflare.New(token, cloudflare.Options{})
}

// pause waits d between guided creation's checks of a new key.
func (e Env) pause(d time.Duration) {
	if e.Pause != nil {
		e.Pause(d)
		return
	}
	time.Sleep(d)
}

// verifyR2Key runs setup's storage check on a key that is not stored yet:
// the same listing and round trip as verifyStorage, without the privacy
// inspection.
func (e Env) verifyR2Key(cfg credentials.Config, key credentials.R2Credentials) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var store storage.ObjectStore
	if e.OpenStore != nil {
		opened, err := e.OpenStore(config.Config{Storage: cfg})
		if err != nil {
			return fmt.Errorf("connect storage: %w", err)
		}
		store = opened
	} else {
		cfg.R2CredentialRef = "guided-new-key"
		opened, err := storage.NewConfiguredStore(ctx, cfg, singleCredential{key})
		if err != nil {
			return fmt.Errorf("connect storage: %w", err)
		}
		store = opened
	}
	if err := storage.Probe(ctx, store); err != nil {
		return err
	}
	return storage.VerifyAccess(ctx, store)
}

// singleCredential is a credential store holding one key in memory, for
// checking a key before it is stored.
type singleCredential struct{ value credentials.R2Credentials }

func (s singleCredential) Save(context.Context, string, credentials.R2Credentials) error {
	return credentials.ErrUnavailable
}

func (s singleCredential) Load(context.Context, string) (credentials.R2Credentials, error) {
	return s.value, nil
}

func (s singleCredential) Delete(context.Context, string) error { return credentials.ErrUnavailable }
