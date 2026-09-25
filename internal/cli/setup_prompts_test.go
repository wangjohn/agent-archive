package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestAppSelectionSuggestionsAndManualFallback(t *testing.T) {
	tests := []struct {
		name     string
		detected []string
		existing []string
		input    string
		want     []string
		prompt   string
		manual   bool
	}{
		{"accept detected", []string{"claude", "codex", "codex"}, nil, "\n", []string{"codex", "claude"}, "Include Codex and Claude Code? [Y/n]", false},
		{"single app", []string{"cursor"}, nil, "y\n", []string{"cursor"}, "Include Cursor? [Y/n]", false},
		{"choose another app", []string{"codex", "claude"}, nil, "n\nn\nn\ny\n", []string{"cursor"}, "Include Codex and Claude Code?", true},
		{"nothing detected", nil, nil, "n\ny\nn\n", []string{"claude"}, "No apps found automatically.", true},
		{"keep prior selection", []string{"claude"}, []string{"claude"}, "\n", []string{"claude"}, "Included: Claude Code. Not included: Codex and Cursor.\nChange which apps are included? [y/N]", false},
		{"add newly found apps", []string{"codex", "claude", "cursor"}, []string{"cursor"}, "\n", []string{"codex", "claude", "cursor"}, "Included: Cursor.\nAlso found on this computer: Codex and Claude Code.\nAdd them? [Y/n]", false},
		{"decline newly found apps", []string{"codex", "claude", "cursor"}, []string{"cursor"}, "n\n\n", []string{"cursor"}, "Also found on this computer: Codex and Claude Code.\nAdd them? [Y/n] Change which apps are included? [y/N]", false},
		{"decline found apps then pick a subset", []string{"codex", "claude", "cursor"}, []string{"cursor"}, "n\ny\nn\ny\ny\n", []string{"claude", "cursor"}, "Change which apps are included?", true},
		{"add one newly found app", []string{"claude"}, []string{"cursor"}, "y\n", []string{"claude", "cursor"}, "Included: Cursor.\nAlso found on this computer: Claude Code.\nAdd it? [Y/n]", false},
		{"add to prior selection", nil, []string{"cursor"}, "y\ny\ny\n\n", []string{"codex", "claude", "cursor"}, "Change which apps are included?", true},
		{"keep full prior selection", nil, []string{"cursor", "claude", "codex"}, "\n", []string{"codex", "claude", "cursor"}, "Keep Codex, Claude Code, and Cursor? [Y/n]", false},
		{"trim full prior selection", nil, []string{"cursor", "claude", "codex"}, "n\nn\n\n\n", []string{"claude", "cursor"}, "Choose which apps to include:", true},
		{"retry empty selection", nil, nil, "n\nn\nn\ny\nn\nn\n", []string{"codex"}, "Choose at least one app to continue.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := promptHarnesses(newPrompter(strings.NewReader(tt.input), &out), tt.detected, tt.existing)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
			if !strings.Contains(out.String(), tt.prompt) {
				t.Fatalf("missing prompt: %s", &out)
			}
			if strings.Contains(out.String(), "Choose which apps to include:") != tt.manual {
				t.Fatalf("incorrect manual fallback: %s", &out)
			}
		})
	}
}

func TestDetectedAppsSetupSkipsIndividualQuestions(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
	input := strings.Join([]string{"y", project, "", "s3", "test-bucket", "profile", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	for _, unwanted := range []string{"Detected settings", "capture policy", "Include Cursor?", "Include Codex?"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("unexpected %q in %s", unwanted, output)
		}
	}
	for _, want := range []string{"Include Codex and Claude Code?", "All new sessions, with or without skills", "90 days, then deleted automatically", "Start archiving?\n  1) Yes, start archiving\n  2) Edit a setting\n  3) Cancel"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in %s", want, output)
		}
	}
}

func TestSetupReviewMarksOnlyChangedValues(t *testing.T) {
	project := t.TempDir()
	old := config.Config{
		Harnesses:     []string{"cursor"},
		RetentionDays: 90,
		Archive:       archive.Config{Projects: []archive.ProjectActivation{{Root: project, Included: true}}},
		Storage:       credentials.Config{Provider: credentials.ProviderR2, Bucket: "agent-archive", R2AccountID: "oldaccount", Prefix: defaultPrefix},
	}
	next := old
	next.Storage.R2AccountID = "newaccount"
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	showSetupReview(p, next, old, true, map[string]applicationDiscovery{"cursor": {Installed: true, Version: "3.21.13", VersionState: "observed"}})
	got := out.String()
	for _, want := range []string{"Apps      Cursor 3.21.13", "* Account   newaccount", "was oldaccount", "* changed from your current settings"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "* ") != 2 || strings.Contains(got, "\x1b[") {
		t.Fatalf("only the account should be marked, with no color in a buffer:\n%s", got)
	}
}

func TestMenuAcceptsNumbersKeysAndPrefixes(t *testing.T) {
	options := []option{{"capture", "Apps and projects"}, {"storage", "Storage"}, {"retention", "Retention"}, {"all", "All settings"}}
	tests := []struct {
		name    string
		input   string
		want    string
		retried bool
	}{
		{"number", "2\n", "storage", false},
		{"blank takes default", "\n", "capture", false},
		{"key", "Retention\n", "retention", false},
		{"unique prefix", "st\n", "storage", false},
		{"out of range retries", "5\n4\n", "all", true},
		{"sentence retries", "edit capture\n1\n", "capture", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := newPrompter(strings.NewReader(tt.input), &out).menu("What would you like to change?", "capture", options...)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q\n%s", got, err, tt.want, &out)
			}
			for _, want := range []string{"  1) Apps and projects\n", "  4) All settings\n", "Enter 1-4 [1]: "} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %s", want, &out)
				}
			}
			if strings.Contains(out.String(), "Enter a number from 1 to 4.") != tt.retried {
				t.Fatalf("unexpected retry behavior: %s", &out)
			}
		})
	}
}

func TestMenuRejectsAmbiguousPrefix(t *testing.T) {
	options := []option{{"retention", "Retention"}, {"region", "Region"}}
	var out bytes.Buffer
	got, err := newPrompter(strings.NewReader("r\n2\n"), &out).menu("Change?", "", options...)
	if err != nil || got != "region" || !strings.Contains(out.String(), "Enter 1-2: ") {
		t.Fatalf("got %q, %v\n%s", got, err, &out)
	}
}

func TestReviewActionMapsChoices(t *testing.T) {
	for input, want := range map[string]string{"1\n": "start", "\n": "start", "2\n": "edit", "3\n": "cancel", "y\n": "start", "n\n": "cancel", "e\n": "edit"} {
		got, err := reviewAction(newPrompter(strings.NewReader(input), &bytes.Buffer{}), false)
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestPickAWSProfileByNumberOrName(t *testing.T) {
	names := []string{"default", "work"}
	for input, want := range map[string]string{"2\n": "work", "\n": "default", "other\n": "other"} {
		var out bytes.Buffer
		got, err := pickAWSProfile(newPrompter(strings.NewReader(input), &out), names, "default")
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q", input, got, err, want)
		}
		if !strings.Contains(out.String(), "  2) work\n") || !strings.Contains(out.String(), "Enter 1-2, or another profile name [1]: ") {
			t.Fatalf("unexpected output: %s", &out)
		}
	}
}

func TestNewlyFoundAppsOmitAppsNotDetected(t *testing.T) {
	var out bytes.Buffer
	got, err := promptHarnesses(newPrompter(strings.NewReader("\n"), &out), []string{"claude", "cursor"}, []string{"cursor"})
	if err != nil || !reflect.DeepEqual(got, []string{"claude", "cursor"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if strings.Contains(out.String(), "Codex") || strings.Contains(out.String(), "Not included") {
		t.Fatalf("an app neither included nor detected was advertised:\n%s", &out)
	}
}

// An app declined at "Also found on this computer" is remembered through the
// setup draft and the saved configuration, is not offered again, and leaves
// the declined list once the user includes it.
func TestSetupRemembersDeclinedApps(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"cursor"} }
	setupRun(t, env, strings.Join([]string{"y", project, "", "s3", "test-bucket", "profile", "us-east-1", "y"}, "\n")+"\n", 0)

	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude", "cursor"} }
	// Apps and projects; decline the found apps; change nothing else; keep
	// the project; add none; cancel at the review so the draft is kept.
	output := setupRun(t, env, "capture\nn\n\ny\n\nn\n", 0)
	if !strings.Contains(output, "Also found on this computer: Codex and Claude Code.\nAdd them? [Y/n]") {
		t.Fatalf("found apps not offered:\n%s", output)
	}
	draft, err := os.ReadFile(filepath.Join(home, "setup-draft.json"))
	if err != nil || !strings.Contains(string(draft), `"declined_harnesses"`) {
		t.Fatalf("draft lost the declined apps: %v\n%s", err, draft)
	}
	// Continue the draft and start archiving.
	setupRun(t, env, "\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"cursor"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex", "claude"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}

	output = setupRun(t, env, "capture\n\ny\n\ny\n", 0)
	if strings.Contains(output, "Also found") || !strings.Contains(output, "Included: Cursor. Not included: Codex and Claude Code.") {
		t.Fatalf("declined apps offered again:\n%s", output)
	}

	// Include Claude Code by hand: it is no longer declined; Codex still is.
	setupRun(t, env, "capture\ny\nn\ny\ny\ny\n\ny\n", 0)
	cfg, _, _ = config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"claude", "cursor"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}
}

// The review screen's app edit has no detection, so it never offers found
// apps, and it keeps the declined list apart from any app it includes.
func TestReviewEditNeverOffersFoundApps(t *testing.T) {
	draft := setupDraft{Config: config.Config{Harnesses: []string{"cursor"}, DeclinedHarnesses: []string{"codex", "claude"}}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("apps\ny\nn\ny\ny\n"), &out)
	if err := editSetupReview(p, &draft, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Also found") || !strings.Contains(out.String(), "Change which apps are included?") {
		t.Fatalf("unexpected prompts:\n%s", &out)
	}
	if !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude", "cursor"}) || !reflect.DeepEqual(draft.Config.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", draft.Config.Harnesses, draft.Config.DeclinedHarnesses)
	}
}

// An app removed by hand in the capture step is declined too, so the next
// reconfigure does not offer it back as found.
func TestSetupRemembersAppRemovedByHand(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
	setupRun(t, env, strings.Join([]string{"y", project, "", "s3", "test-bucket", "profile", "us-east-1", "y"}, "\n")+"\n", 0)

	// Apps and projects; change apps: Codex no, Claude Code yes, Cursor no;
	// keep the project; add none; start archiving.
	setupRun(t, env, "capture\ny\nn\ny\nn\ny\n\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"claude"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}

	output := setupRun(t, env, "capture\n\ny\n\nn\n", 0)
	if strings.Contains(output, "Also found") || !strings.Contains(output, "Included: Claude Code. Not included: Codex and Cursor.") {
		t.Fatalf("removed app offered again:\n%s", output)
	}
}

// An app removed in the review screen's app edit is declined, and a later
// capture step with it detected does not offer it back.
func TestReviewEditRemovalIsNotOfferedAgain(t *testing.T) {
	draft := setupDraft{Config: config.Config{Harnesses: []string{"codex", "claude"}}}
	p := newPrompter(strings.NewReader("apps\ny\nn\ny\nn\n"), &bytes.Buffer{})
	if err := editSetupReview(p, &draft, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude"}) || !reflect.DeepEqual(draft.Config.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", draft.Config.Harnesses, draft.Config.DeclinedHarnesses)
	}
	var out bytes.Buffer
	if err := chooseHarnesses(newPrompter(strings.NewReader("\n"), &out), []string{"codex", "claude"}, &draft.Config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Also found") || !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude"}) {
		t.Fatalf("removed app offered again: %v\n%s", draft.Config.Harnesses, &out)
	}
}

func TestSetupReviewShowsDeclinedApps(t *testing.T) {
	old := config.Config{Harnesses: []string{"cursor"}, RetentionDays: 90, Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2AccountID: "a", Prefix: defaultPrefix}}
	next := old
	next.DeclinedHarnesses = []string{"codex", "claude"}
	var out bytes.Buffer
	showSetupReview(newPrompter(strings.NewReader(""), &out), next, old, true, nil)
	got := out.String()
	if !strings.Contains(got, "* Skipped   Codex and Claude Code (setup will not offer again)") || strings.Contains(got, "Nothing above differs") {
		t.Fatalf("declining found apps not shown as a change:\n%s", got)
	}
}
