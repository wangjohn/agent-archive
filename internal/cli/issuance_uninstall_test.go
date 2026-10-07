package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestUninstallPurgeRefusesDuringDedicatedCreation(t *testing.T) {
	env, home, cf, kc := dedicatedFixture(t)
	issuer := fixtureIssuer(t, env, home)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	defer release()
	api := issuer.api
	started, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unpause := releaseOnce(func() { close(resume) })
	issuer.api = &trackedAPI{API: api, createToken: func(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
		close(started)
		<-resume
		return api.CreateToken(ctx, account, spec)
	}}
	var slot issuance.Slot
	var createErr error
	go func() {
		defer close(finished)
		slot, _, createErr = issuer.create(issuance.Precreated)
	}()
	defer func() { unpause(); <-finished }()
	<-started
	// Creation intent is durable, but the provider has not returned its key.
	var out, errOut bytes.Buffer
	if status := runUninstallCommand([]string{"--delete-local-data", "--yes"}, nil, &out, &errOut, env); status != 1 || !strings.Contains(errOut.String(), "issuance") {
		t.Fatalf("purge during creation: status=%d out=%s err=%s", status, &out, &errOut)
	}
	cfg, found, err := config.Load(home)
	must(t, err)
	if !found || !cfg.Archive.Enabled {
		t.Fatal("purge changed configuration while issuance was active")
	}
	if _, err := kc.Load(context.Background(), "main"); err != nil {
		t.Fatal("purge deleted the source credential")
	}
	slots, err := issuance.List(home)
	must(t, err)
	if len(slots) != 1 || slots[0].State != issuance.CreationIntent {
		t.Fatal("purge removed creation lineage")
	}
	if _, err := os.Stat(filepath.Join(home, "issued.lock")); err != nil {
		t.Fatal("purge unlinked the active issuance lock")
	}
	unpause()
	<-finished
	must(t, createErr)
	slots, err = issuance.List(home)
	must(t, err)
	if cf.Calls(cloudflaretest.RouteCreateToken) != 1 || len(cf.Live()) != 1 || len(slots) != 1 || slots[0].ProviderID != slot.ProviderID || slots[0].State != issuance.Spare {
		t.Fatal("creation did not retain its successful provider lineage")
	}
}

func TestUninstallPurgeRemovesIdleIssuanceLockAndDirectory(t *testing.T) {
	env, home, _ := pairingSourceFixture(t)
	release, err := local.NamedLock(home, "issued.lock")
	must(t, err)
	release()
	var out, errOut bytes.Buffer
	if status := runUninstallCommand([]string{"--delete-local-data", "--yes"}, nil, &out, &errOut, env); status != 0 {
		t.Fatalf("purge status=%d out=%s err=%s", status, &out, &errOut)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("purge retained data directory: %v", err)
	}
}

func TestPairingAddRejectsSourceChangedBeforeIssuanceLock(t *testing.T) {
	type sourceChange string
	const (
		purged      sourceChange = "purged"
		disabled    sourceChange = "disabled"
		destination sourceChange = "destination"
		capture     sourceChange = "capture"
	)
	for _, change := range []sourceChange{purged, disabled, destination, capture} {
		t.Run(string(change), func(t *testing.T) {
			env, home, cf, _ := dedicatedFixture(t)
			cfg, _, userHome, err := preparePairingSource("laptop", false, env)
			must(t, err)
			payload, err := sourcePairingPayload(cfg, "laptop", userHome, 15*time.Minute, env)
			must(t, err)
			current, _, err := config.Load(home)
			must(t, err)
			switch change {
			case purged:
				// A purge preserves unrelated files and their directory, but removes all
				// owned configuration and the old lock inode. A waiting add can reopen it.
				must(t, os.WriteFile(filepath.Join(home, "unrelated-note"), []byte("keep"), 0600))
				var out, errOut bytes.Buffer
				if status := runUninstallCommand([]string{"--delete-local-data", "--yes"}, nil, &out, &errOut, env); status != 1 || !strings.Contains(errOut.String(), "unrelated-note") {
					t.Fatalf("purge status=%d out=%s err=%s", status, &out, &errOut)
				}
				if _, found, err := config.Load(home); err != nil || found {
					t.Fatal("purge retained configuration")
				}
			case disabled:
				current.Archive.Enabled = false
				must(t, config.Save(home, current))
			case destination:
				current.Storage.Bucket = "replacement"
				must(t, config.Save(home, current))
			case capture:
				current.RetentionDays++
				must(t, config.Save(home, current))
			}
			originalLookup := env.LookupEnv
			env.LookupEnv = func(name string) (string, bool) {
				if name == "CLOUDFLARE_API_TOKEN" {
					return bootstrapCanary, true
				}
				return originalLookup(name)
			}
			var out, errOut bytes.Buffer
			status := executePairingAdd(home, cfg, payload, newPrompter(nil, &out), env, &out, &errOut, pairingAddOptions{userHome: t.TempDir(), name: "laptop", spares: -1, yes: true, printBundle: true})
			if status != 1 || cf.Calls(cloudflaretest.RouteCreateToken) != 0 || len(cf.Live()) != 0 {
				t.Fatalf("stale source minted or delivered: status=%d calls=%d out=%s err=%s", status, cf.Calls(cloudflaretest.RouteCreateToken), &out, &errOut)
			}
			slots, err := issuance.List(home)
			must(t, err)
			if len(slots) != 0 {
				t.Fatal("stale source created issuance lineage")
			}
		})
	}
}
