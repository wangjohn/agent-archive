package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestProtectedSetupDraftPersistsChangedSkillEvidence(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{true, false} {
		t.Run(map[bool]string{true: "interactive-review", false: "unattended-key-staging"}[interactive], func(t *testing.T) {
			t.Parallel()
			for _, enabled := range []bool{true, false} {
				home := t.TempDir()
				at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
				cfg := pairTestConfig(at, []string{"codex"}, t.TempDir())
				cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}
				must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
				cfg.Discovery.Enabled = enabled
				must(t, config.Save(home, cfg))
				cfg = mustLoadConfig(t, home)
				if !interactive {
					cfg.SkillEvidence = config.SkillEvidenceNone
				}
				draft := setupDraft{Version: draftFormat, Config: cfg, Step: 2}
				if interactive {
					var out bytes.Buffer
					p := newPrompter(strings.NewReader("skills\nnone\n"), &out)
					must(t, editSetupReview(p, &draft, t.TempDir(), nil, nil))
				}
				before := draft.Config
				save := func() error { return local.Write(draftPath(home), draft) }
				if interactive {
					must(t, save())
				} else {
					keychain := newFakeKeychain()
					env := testEnv(t, home, at)
					env.Credentials = func() (credentials.CredentialStore, error) { return keychain, nil }
					storage := credentials.Config{Provider: credentials.ProviderR2, Bucket: "synthetic"}
					secret := credentials.R2Credentials{AccessKeyID: "synthetic-id", SecretAccessKey: "synthetic-secret"}
					must(t, stageStorageSecret(&draft, save, env, &storage, secret))
					stored, err := keychain.Load(context.Background(), draft.CredentialRef)
					if err != nil || stored != secret {
						t.Fatal("staged credential not persisted after draft", err)
					}
					before.Storage = draft.Config.Storage
				}
				if !reflect.DeepEqual(before, draft.Config) {
					t.Fatal("wire normalization mutated the in-memory configuration")
				}
				loaded, found, problem, err := readDraft(home)
				if err != nil || !found || problem != "" || loaded.Config.EffectiveSkillEvidence() != config.SkillEvidenceNone || !reflect.DeepEqual(loaded.Config.Discovery, cfg.Discovery) {
					t.Fatalf("draft lost policy or authorization: %#v %t %s %v", loaded, found, problem, err)
				}
				raw, err := os.ReadFile(draftPath(home))
				must(t, err)
				var published struct {
					Config struct {
						SchemaVersion int `json:"schema_version"`
					} `json:"config"`
				}
				if json.Unmarshal(raw, &published) == nil {
					t.Fatal("saved draft downgraded the published-writer fence")
				}
			}
		})
	}
}
