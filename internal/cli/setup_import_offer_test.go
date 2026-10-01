package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// setupImportAnswers set Claude Code up in ~/src/web-app, storing in S3,
// and answer the import offer with importAnswer.
func setupImportAnswers(importAnswer string) string {
	return strings.Join([]string{"", "2", "work", "2", "", importAnswer}, "\n") + "\n"
}

// newImportOfferFixture is a Mac with Claude Code, run from ~/src/web-app,
// with two past sessions there and one in a project setup leaves out.
func newImportOfferFixture(t *testing.T) *screenFixture {
	t.Helper()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	f.pastSession(t, "one", "src/web-app", screenNow.Add(-72*time.Hour))
	f.pastSession(t, "two", "src/web-app", screenNow.Add(-2*time.Hour))
	f.pastSession(t, "other", "src/api", screenNow.Add(-2*time.Hour))
	return f
}

func (f *screenFixture) runSetup(t *testing.T, input string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if code := Run(append([]string{"setup"}, args...), strings.NewReader(input), &out, &out, f.env); code != 0 {
		t.Fatalf("setup exit %d\n%s", code, &out)
	}
	return out.String()
}

// importedSessions lists the native IDs of the sessions an import
// registered.
func importedSessions(t *testing.T, home string) []string {
	t.Helper()
	batches, err := backfill.LoadBatches(home)
	must(t, err)
	var ids []string
	for _, b := range batches {
		parents, _ := importRegistrations(t, home, b.ID)
		for _, reg := range parents {
			ids = append(ids, reg.NativeSessionID)
		}
	}
	return ids
}

// Setup ends by offering to import the chosen projects' past sessions; yes
// imports them, and only them, as an import backfill undo removes again.
func TestSetupImportsPastSessionsWhenAsked(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	out := f.runSetup(t, setupImportAnswers("y"))
	if !strings.Contains(out, "Import the 2 past sessions from these projects? [Y/n]") || !strings.Contains(out, "Uploaded 2 sessions") {
		t.Fatalf("no import:\n%s", out)
	}
	ids := importedSessions(t, f.home)
	if strings.Join(ids, ",") != "one,two" && strings.Join(ids, ",") != "two,one" {
		t.Fatalf("imported %v", ids)
	}
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(batches) != 1 || batches[0].CompletedAt == nil {
		t.Fatalf("import record %+v", batches)
	}
	regs, _ := importRegistrations(t, f.home, batches[0].ID)
	var undoOut bytes.Buffer
	if code := Run([]string{"backfill", "undo", "--yes", batches[0].ID}, strings.NewReader(""), &undoOut, &undoOut, f.env); code != 0 {
		t.Fatalf("undo exit %d\n%s", code, &undoOut)
	}
	if left := importedSessions(t, f.home); len(left) != 0 {
		t.Fatalf("still registered after undo: %v", left)
	}
	for _, reg := range regs {
		if keys := sessionKeys(t, f.bucket, reg); len(keys) != 0 {
			t.Fatalf("%s left in the bucket: %v", reg.ArchiveSessionID, keys)
		}
	}
	// Undo leaves the project setup chose.
	cfg, _, err := config.Load(f.home)
	must(t, err)
	if includedProjects(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != filepath.Join(f.userHome, "src", "web-app") {
		t.Fatalf("projects after undo %+v", cfg.Archive.Projects)
	}
}

// No leaves the past sessions where they are and says how to import them.
func TestSetupLeavesPastSessionsWhenDeclined(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	out := f.runSetup(t, setupImportAnswers("n"))
	if !strings.Contains(out, "Import the 2 past sessions") || !strings.Contains(out, "Not imported. Import them later with agent-archive backfill.") {
		t.Fatalf("offer:\n%s", out)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
	if _, err := os.Stat(filepath.Join(f.home, "imports")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(f.home, "imports"))
		if len(entries) != 0 {
			t.Fatalf("import records written: %v", entries)
		}
	}
}

// With nothing to import, setup asks nothing.
func TestSetupAsksNothingWithNothingToImport(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	// A session in a project setup leaves out is not offered.
	f.pastSession(t, "other", "src/api", screenNow.Add(-2*time.Hour))
	out := f.runSetup(t, strings.TrimSuffix(setupImportAnswers(""), "\n"))
	if strings.Contains(out, "Import the") || !strings.Contains(out, "Looking for past sessions in these projects… none to import.") {
		t.Fatalf("offer:\n%s", out)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// While capture is paused, setup offers no import: backfill would refuse it.
func TestSetupWhilePausedOffersNoImport(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.installed(t)
	var out bytes.Buffer
	if code := Run([]string{"pause"}, nil, &out, &out, f.env); code != 0 {
		t.Fatalf("pause: %s", &out)
	}
	f.pastSession(t, "one", "src/web-app", screenNow.Add(-2*time.Hour))
	// Change retention, and save.
	got := f.runSetup(t, "3\n30\n\n")
	if strings.Contains(got, "Looking for past sessions") || strings.Contains(got, "Import the") || !strings.Contains(got, "agent-archive resume") {
		t.Fatalf("offer while paused:\n%s", got)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// setup --yes asks nothing and imports nothing; it points at backfill.
func TestSetupYesPointsAtBackfill(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.project(t, "src/web-app")
	f.pastSession(t, "one", "src/web-app", screenNow.Add(-2*time.Hour))
	out := f.runSetup(t, "", "--yes", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--apps", "claude", "--project", "~/src/web-app")
	if strings.Contains(out, "Import the") || strings.Contains(out, "Looking for past sessions") || !strings.Contains(out, "Import sessions from before setup with agent-archive backfill.") {
		t.Fatalf("output:\n%s", out)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// The line for another machine names the R2 key only by its environment
// variables, never by value, whether setup asked for it or read it.
func TestAnotherMachineCommandNeverCarriesTheR2Secret(t *testing.T) {
	t.Parallel()
	const secret, keyID = "private-secret-value", "PRIVATEKEYID"
	check := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, secret) || strings.Contains(out, keyID) {
			t.Fatalf("output carries the key:\n%s", out)
		}
		want := "To set up another machine with this storage, set " + envR2AccessKeyID + " and\n" + envR2SecretAccessKey + " there, then run:\n  agent-archive setup --yes --provider r2 --bucket test-bucket --r2-account " + testR2Account + " --apps codex --project "
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	t.Run("interactive", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
		input := strings.Join([]string{"y", "n", "n", t.TempDir(), "", "r2", testR2Account, "test-bucket", keyID, secret, "y"}, "\n") + "\n"
		check(t, setupRun(t, env, input, 0))
	})
	t.Run("yes", func(t *testing.T) {
		t.Parallel()
		env := withEnvironment(setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Now()), map[string]string{envR2AccessKeyID: keyID, envR2SecretAccessKey: secret})
		var out bytes.Buffer
		args := []string{"setup", "--yes", "--provider", "r2", "--r2-account", testR2Account, "--bucket", "test-bucket", "--apps", "codex", "--project", t.TempDir()}
		if code := Run(args, strings.NewReader(""), &out, &out, env); code != 0 {
			t.Fatalf("exit %d\n%s", code, &out)
		}
		check(t, out.String())
	})
}

// The command for another machine writes projects in the home folder from ~, and
// quotes what the shell would split.
func TestAnotherMachineCommand(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Storage:   credentials.Config{Provider: credentials.ProviderS3, Bucket: "team-archive", AWSProfile: "work", Region: "us-east-1"},
		Harnesses: []string{"codex", "claude"},
		Archive: archive.Config{Projects: []archive.ProjectActivation{
			{Root: "/Users/alex/src/web app", Included: true},
			{Root: "/Users/alex/src/api", Included: false},
			{Root: "/Volumes/work/it's", Included: true},
			{Root: "/Users/alex", Included: true},
		}},
	}
	got := anotherMachineCommand(cfg, "/Users/alex")
	want := `agent-archive setup --yes --provider s3 --bucket team-archive --aws-profile work --region us-east-1 --apps codex,claude --project '~/src/web app' --project '/Volumes/work/it'\''s' --project ~`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A project in the home folder is written from ~ even when the home folder's
// path runs through a symlink, as project roots are saved resolved.
func TestAnotherMachineCommandResolvesTheHomeFolder(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	must(t, os.Symlink(target, link))
	resolved, err := filepath.EvalSymlinks(target)
	must(t, err)
	cfg := config.Config{
		Storage:   credentials.Config{Provider: credentials.ProviderS3, Bucket: "b", AWSProfile: "p"},
		Harnesses: []string{"claude"},
		Archive:   archive.Config{Projects: []archive.ProjectActivation{{Root: filepath.Join(resolved, "src", "app"), Included: true}}},
	}
	if got := anotherMachineCommand(cfg, link); !strings.HasSuffix(got, " --project ~/src/app") {
		t.Fatalf("got %s", got)
	}
}

// An import that fails keeps the setup it follows, and names the backfill
// command that finishes that same import.
func TestSetupImportFailureKeepsSetupAndNamesTheRetry(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	f.env.backfillHoldSteps = 1
	crashed := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !crashed {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	out := f.runSetup(t, setupImportAnswers("y"))
	retry := "agent-archive backfill --harness claude --project ~/src/web-app"
	if !strings.Contains(out, "Setup is complete. To finish the import, run "+retry+".") {
		t.Fatalf("no retry:\n%s", out)
	}
	cfg, found, err := config.Load(f.home)
	must(t, err)
	if !found || !cfg.Archive.Enabled || includedProjects(cfg.Archive.Projects) != 1 {
		t.Fatalf("setup not kept: %+v", cfg)
	}
	if _, e := os.Stat(draftPath(f.home)); !os.IsNotExist(e) {
		t.Fatalf("draft left: %v", e)
	}
	f.env.backfillCheckpoint = nil
	var rerun bytes.Buffer
	args := []string{"backfill", "--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "src", "web-app")}
	if code := Run(args, strings.NewReader(""), &rerun, &rerun, f.env); code != 0 {
		t.Fatalf("retry exit %d\n%s", code, &rerun)
	}
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(batches) != 1 || batches[0].CompletedAt == nil {
		t.Fatalf("the retry did not finish setup's import: %+v\n%s", batches, &rerun)
	}
	if ids := importedSessions(t, f.home); len(ids) != 2 {
		t.Fatalf("imported %v", ids)
	}
}

// When an import registers every session and only its upload falls behind,
// the background collector finishes it: no retry is named.
func TestSetupImportUploadFailureNamesNoRetry(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "uploading" {
			return errors.New("simulated crash")
		}
		return nil
	}
	out := f.runSetup(t, setupImportAnswers("y"))
	if strings.Contains(out, "To finish the import") || !strings.Contains(out, "Registered 2 sessions") {
		t.Fatalf("output:\n%s", out)
	}
}

// The command for another machine names an R2 bucket on a custom endpoint by
// that endpoint, and says how to set a folder inside the bucket, which
// setup --yes cannot.
func TestAnotherMachineCommandCustomEndpointAndFolder(t *testing.T) {
	t.Parallel()
	endpoint := "https://" + testR2Account + ".eu.r2.cloudflarestorage.com"
	cfg := config.Config{
		Storage: credentials.Config{
			Provider: credentials.ProviderR2, Bucket: "b", Prefix: "team/",
			R2Endpoint: endpoint, R2CredentialRef: "ref-123",
		},
		Harnesses: []string{"claude"},
		Archive:   archive.Config{Projects: []archive.ProjectActivation{{Root: "/Users/alex/src/app", Included: true}}},
	}
	var out bytes.Buffer
	printAnotherMachine(newPrompter(strings.NewReader(""), &out), cfg, "/Users/alex")
	got := out.String()
	want := "  agent-archive setup --yes --provider r2 --bucket b --r2-account " + endpoint + " --apps claude --project ~/src/app\nThen run agent-archive setup there and set the folder inside the bucket to team/.\n"
	if !strings.HasSuffix(got, want) || strings.Contains(got, "ref-123") {
		t.Fatalf("got:\n%s", got)
	}
	if loc, err := credentials.ParseR2Location(endpoint); err != nil || loc.Endpoint != endpoint {
		t.Fatalf("the endpoint does not read back: %+v %v", loc, err)
	}
}
