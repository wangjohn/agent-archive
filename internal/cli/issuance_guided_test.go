package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Regression: ISSUE-R1-01. Review cancellation is not a configuration commit.
func TestGuidedDedicatedStagingRemainsOwnIntentUntilCommit(t *testing.T) {
	g := newGuidedR2Fixture(t)
	g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "", "no")...), 0)
	slots, err := issuance.List(g.home)
	must(t, err)
	own := 0
	for _, slot := range slots {
		if slot.Origin == issuance.Guided {
			own++
			if slot.State != issuance.OwnIntent || !strings.HasPrefix(slot.SecretRef, "setup-") {
				t.Fatalf("uncommitted slot: %+v", slot)
			}
		}
	}
	if own != 1 {
		t.Fatal("missing guided stage")
	}
	cfg, _, err := config.Load(g.home)
	must(t, err)
	if cfg.MachineAssignment != nil {
		t.Fatal("cancel committed assignment")
	}
}

// Regression: ISSUE-R1-02. An unavailable one-time value must not mint twice.
func TestGuidedDedicatedRetryWithUnresolvedCreationNeverDuplicates(t *testing.T) {
	env, home, cf, _ := dedicatedFixture(t)
	api := env.cloudflareAPI(bootstrapCanary)
	calls := 0
	tracked := &trackedAPI{API: api, createToken: func(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
		calls++
		_, err := api.CreateToken(ctx, account, spec)
		must(t, err)
		return cloudflare.Token{}, errors.New("synthetic lost reply")
	}}
	defer tracked.Discard()
	c := r2Creator{home: home, env: env, p: newPrompter(strings.NewReader(""), &bytes.Buffer{}), api: tracked, account: cloudflaretest.AccountID, bucket: cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: "synthetic"}}, bucketCreated: true, groupID: "aaaa0000000000000000000000000002"}
	_, err := c.attemptWith(context.Background())
	if err == nil {
		t.Fatal("lost reply accepted")
	}
	_, err = c.attemptWith(context.Background())
	if err == nil || calls != 1 || len(cf.Live()) != 1 {
		t.Fatalf("retry duplicated unresolved creation: calls=%d err=%v", calls, err)
	}
}

// Regression: ISSUE-R1-01. Reconciliation cannot race verification/staging.
func TestGuidedDedicatedHoldsIssuanceLockThroughStaging(t *testing.T) {
	g := newGuidedR2Fixture(t)
	stages := 0
	g.env.Credentials = func() (credentials.CredentialStore, error) {
		return &hookKeychain{fakeKeychain: g.keychain, onSave: func(ref string) {
			if !strings.HasPrefix(ref, "setup-") {
				return
			}
			stages++
			release, err := local.NamedLock(g.home, "issued.lock")
			if err == nil {
				release()
				t.Fatal("credential staging has no issuance lock")
			}
		}}, nil
	}
	g.createToken = func(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
		release, err := local.NamedLock(g.home, "issued.lock")
		if err == nil {
			release()
			t.Fatal("creation has no issuance lock")
		}
		return cloudflare.New(bootstrapCanary, cloudflare.Options{BaseURL: g.cf.URL + "/client/v4"}).CreateToken(ctx, account, spec)
	}
	g.run(t, g.happy(), 0)
	if stages != 1 {
		t.Fatal("missing staged credential")
	}
	slots, err := issuance.List(g.home)
	must(t, err)
	for _, slot := range slots {
		if slot.Origin == issuance.Guided && slot.State != issuance.Own {
			t.Fatal("committed own stage not promoted")
		}
	}
	release, err := local.NamedLock(g.home, "issued.lock")
	must(t, err)
	release()
}

// Regression: ISSUE-R1-01. Discard keeps explicit provider cleanup lineage.
func TestGuidedDedicatedDiscardTracksProviderCleanup(t *testing.T) {
	g := newGuidedR2Fixture(t)
	g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "n", "", "no")...), 0)
	draft, have, problem, err := readDraft(g.home)
	must(t, err)
	if !have || problem != "" {
		t.Fatal("missing guided draft")
	}
	cfg, _, err := config.Load(g.home)
	must(t, err)
	must(t, discardDraft(g.home, draft, cfg, g.env))
	slots, err := issuance.List(g.home)
	must(t, err)
	for _, slot := range slots {
		if slot.Origin == issuance.Guided && slot.State != issuance.CleanupPending {
			t.Fatal("discard lost cleanup ownership")
		}
	}
	if len(g.cf.Live()) != 3 {
		t.Fatal("discard implicitly touched provider")
	}
}
