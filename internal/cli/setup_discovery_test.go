package cli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestFreshCodexSetupCommitsDiscoveryConsent(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	output := setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project)
	cfg := mustLoadConfig(t, home)
	if cfg.Discovery == nil || !cfg.Discovery.Enabled || len(cfg.Discovery.Authorizations) != 1 {
		t.Fatalf("discovery consent missing: %+v", cfg.Discovery)
	}
	a := cfg.Discovery.Authorizations[0]
	if len(a.Intervals) != 1 || !a.Intervals[0].Start.Equal(at) || a.DestinationID != cfg.DestinationID() || a.ProjectRoot != project {
		t.Fatalf("wrong committed consent: %+v", a)
	}
	if !strings.Contains(output, "Qualifying recent native copies") {
		t.Fatalf("copy limitation missing: %s", output)
	}
	before := cfg.Discovery
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	cfg = mustLoadConfig(t, home)
	if !reflect.DeepEqual(before, cfg.Discovery) {
		t.Fatal("unrelated setup changed discovery generation")
	}
	setupYes(t, env, "", 0, "--yes", "--codex-discovery", "off")
	cfg = mustLoadConfig(t, home)
	if cfg.Discovery == nil || cfg.Discovery.Enabled {
		t.Fatal("explicit off not committed")
	}
}

func TestExistingHookOnlySetupRequiresDiscoveryOptIn(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project, "--codex-discovery", "off")
	cfg := mustLoadConfig(t, home)
	// Identity initialization may require a disabled protected configuration;
	// either representation must remain non-consenting on ordinary upgrades.
	if cfg.Discovery != nil && cfg.Discovery.Enabled {
		t.Fatal("off silently enabled")
	}
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	cfg = mustLoadConfig(t, home)
	if cfg.Discovery != nil && cfg.Discovery.Enabled {
		t.Fatal("upgrade silently enabled")
	}
	setupYes(t, env, "", 0, "--yes", "--codex-discovery", "on")
	cfg = mustLoadConfig(t, home)
	if cfg.Discovery == nil || !cfg.Discovery.Enabled || len(cfg.Discovery.Authorizations) != 1 {
		t.Fatal("explicit opt-in not committed")
	}
}

func TestDiscoveryUpgradePromptDefaultsToNo(t *testing.T) {
	t.Parallel()
	for _, protected := range []bool{false, true} {
		for _, answer := range []string{"\n", "y\n"} {
			var discovery *config.DiscoveryConfig
			if protected {
				discovery = &config.DiscoveryConfig{}
			}
			previous := config.Config{Harnesses: []string{"codex"}, Discovery: discovery}
			if err := config.SetCodexCaptureScope(&previous, config.CodexIncludedProjects); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			draft := setupDraft{Config: previous}
			p := newPrompter(strings.NewReader(answer), &out)
			if err := promptDiscovery(p, &draft, previous); err != nil {
				t.Fatal(err)
			}
			enabled := draft.Config.Discovery != nil && draft.Config.Discovery.Enabled
			if draft.Config.Discovery == nil || enabled != (answer == "y\n") || !draft.Config.Discovery.ChoiceRecorded {
				t.Fatalf("answer %q enabled %v", answer, enabled)
			}
			if !strings.Contains(out.String(), "2) No (default)") {
				t.Fatalf("opt-in default unclear: %s", &out)
			}
			// Explicit off and on survive an unrelated later setup without another
			// consent prompt; low-level compatibility protection alone never does.
			var nextOut bytes.Buffer
			next := setupDraft{Config: draft.Config}
			if err := promptDiscovery(newPrompter(strings.NewReader(""), &nextOut), &next, draft.Config); err != nil {
				t.Fatal(err)
			}
			if nextOut.Len() != 0 {
				t.Fatal("recorded choice prompted again")
			}
		}
	}
}

func TestDiscoveryStatusDoesNotRequireHookApproval(t *testing.T) {
	t.Parallel()
	view := statusView{Background: "loaded", Apps: []appStatus{{Name: "codex", Hooks: "missing or incomplete", Discovery: &discovery.Health{Enabled: true}, Projects: []projectCaptureStatus{{ProjectRoot: "/synthetic/project"}}}}}
	chooseCaptureStep(&view)
	if !strings.Contains(view.Next, "Hook approval is optional") {
		t.Fatalf("next step requires hooks: %s", view.Next)
	}
	chooseInstallationStep(&view, statusBackground{})
	if strings.Contains(view.problem, "hooks aren't installed") {
		t.Fatal("missing optional hooks blocked discovery")
	}
}

func TestReviewSkillPolicyEditRetainsDiscoveryDraftProtection(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := config.Config{Harnesses: []string{"codex"}, SkillEvidence: config.SkillEvidenceMetadata}
	config.SetDiscoveryChoice(&cfg, true)
	draft := setupDraft{Version: draftFormat, Config: cfg}
	var out bytes.Buffer
	if err := editSetupReview((Env{}).setupNames(), newPrompter(strings.NewReader("skills\nnone\n"), &out), &draft, t.TempDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(draftPath(home), draft); err != nil {
		t.Fatal(err)
	}
	read, found, problem, err := readDraft(home)
	if err != nil || !found || problem != "" {
		t.Fatalf("protected draft: %v %s %v", found, problem, err)
	}
	if read.Config.EffectiveSkillEvidence() != config.SkillEvidenceNone || read.Config.Discovery == nil || !read.Config.Discovery.Enabled || !read.Config.Discovery.ChoiceRecorded || len(read.Config.Discovery.Authorizations) != 0 {
		t.Fatalf("skill edit altered proposed consent: %+v", read.Config)
	}
}
