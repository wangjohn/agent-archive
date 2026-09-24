package cli

import (
	"bytes"
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
		name               string
		detected, existing []string
		input              string
		want               []string
		prompt             string
		manual             bool
	}{
		{"accept detected", []string{"claude", "codex", "codex"}, nil, "\n", []string{"codex", "claude"}, "Include Codex and Claude Code? [Y/n]", false},
		{"single app", []string{"cursor"}, nil, "y\n", []string{"cursor"}, "Include Cursor? [Y/n]", false},
		{"choose another app", []string{"codex", "claude"}, nil, "n\nn\nn\ny\n", []string{"cursor"}, "Include Codex and Claude Code?", true},
		{"nothing detected", nil, nil, "n\ny\nn\n", []string{"claude"}, "No apps found automatically.", true},
		{"keep prior selection", []string{"codex", "cursor"}, []string{"claude"}, "\n", []string{"claude"}, "Keep Claude Code?", false},
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
	for _, want := range []string{"Include Codex and Claude Code?", "All new sessions, with or without skills", "90 days, then deleted automatically", "Start archiving? [Y/n/edit]"} {
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
