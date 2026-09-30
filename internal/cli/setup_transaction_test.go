package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

func TestMergeCommittedSetupStateKeepsOperationalOwnership(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := config.Config{
		DestinationSince:      since,
		PreviousDestinations:  []credentials.Config{{Provider: credentials.ProviderS3, Bucket: "prior"}},
		ImportedHarnesses:     []string{"claude", "codex", "cursor"},
		RetiredCredentialRefs: []string{"old-ref"},
	}
	next := config.Config{
		Harnesses:             []string{"claude"},
		SkillEvidence:         config.SkillEvidenceMetadata,
		RetiredCredentialRefs: []string{"new-ref"},
	}
	mergeCommittedSetupState(old, &next, []string{"cursor"})
	if !next.DestinationSince.Equal(since) || !reflect.DeepEqual(next.ImportedHarnesses, []string{"codex"}) ||
		!reflect.DeepEqual(next.RetiredCredentialRefs, []string{"new-ref", "old-ref"}) ||
		next.SkillEvidence != config.SkillEvidenceMetadata {
		t.Fatalf("merged setup state = %+v", next)
	}
	old.PreviousDestinations[0].Bucket = "changed"
	if len(next.PreviousDestinations) != 1 || next.PreviousDestinations[0].Bucket != "prior" {
		t.Fatalf("previous destinations alias committed state: %+v", next.PreviousDestinations)
	}
}

func TestSetupFailureBeforeCommitRecoversFreshInstall(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("bootstrap failed") }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 1)
	if _, found, _ := config.Load(home); found {
		t.Fatal("failed fresh setup left active config")
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup left hooks")
	}
	if setupjournal.TransactionPending(home) {
		t.Fatal("journal not cleaned after restoration")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestResumeDraftLeftAfterDestinationCommitPreservesOwnership(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("original", "us-east-1", "profile", true, false, false, project), 0)
	old, _, _ := config.Load(home)
	changed := old
	changed.Storage.Bucket = "new-bucket"
	stale := setupDraft{Version: 1, Step: 2, Config: changed}
	// Persist the pre-commit draft, then commit without the wizard's deletion:
	// exactly the state left by a crash or failed draft removal after apply.
	if err := local.Write(filepath.Join(home, "setup-draft.json"), stale); err != nil {
		t.Fatal(err)
	}
	next := stale.Config
	exe, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySetup(home, userHome, exe, old, &next, nil, env); err != nil {
		t.Fatal(err)
	}
	committed, _, _ := config.Load(home)
	if committed.DestinationSince.IsZero() {
		t.Fatal("switch did not establish boundary")
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	setupRun(t, env, "continue\ny\n", 0)
	resumed, _, _ := config.Load(home)
	if !resumed.DestinationSince.Equal(committed.DestinationSince) || !reflect.DeepEqual(resumed.PreviousDestinations, committed.PreviousDestinations) {
		t.Fatalf("ownership changed: before=%+v after=%+v", committed, resumed)
	}
	if resumed.AcceptSession(archive.SessionRegistration{SessionStartedAt: now.Add(-time.Minute), Harness: archive.Harness{Name: "codex"}, ProjectRoot: resumed.Archive.Projects[0].Root}) {
		t.Fatal("retired session became eligible")
	}
}

// Shortening retention warns with the number of sessions it makes
// deletable, counted in words, and the cutoff date; when there are none,
// it says nothing.
func TestShorterRetentionWarnsOnlyWhenSessionsBecomeDeletable(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.installed(t)
	f.published(t)
	old, _, err := config.Load(f.home)
	must(t, err)
	next := old
	next.RetentionDays = 30
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{screenNow, ""},
		{screenNow.Add(60 * 24 * time.Hour), "! Shorter retention: 1 session captured on or before 2026-10-25 will be eligible for deletion.\n    Future cleanup also applies this policy.\n"},
		// The date is the user's: the same cutoff, 12:00 UTC, is still the
		// evening before 13 hours west of UTC.
		{screenNow.Add(60 * 24 * time.Hour).In(time.FixedZone("UTC-13", -13*3600)), "! Shorter retention: 1 session captured on or before 2026-10-24 will be eligible for deletion.\n"},
	} {
		var out bytes.Buffer
		env := f.env
		env.Now = func() time.Time { return tc.now }
		p := newPrompter(strings.NewReader(""), &out)
		must(t, reviewChanges(f.home, old, next, p, env))
		if got := out.String(); !strings.Contains(got, tc.want) || (tc.want == "" && got != "") {
			t.Errorf("at %s: output %q, want %q", tc.now.Format(time.DateOnly), got, tc.want)
		}
	}
}
