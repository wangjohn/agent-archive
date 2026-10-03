package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/wangjohn/agent-archive/internal/config"
	"strings"
	"testing"
	"time"
)

func TestManagementTokenSourcesAreExplicitAndFailClosed(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	calls, removed := 0, false
	env.LookupEnv = func(key string) (string, bool) { return "ENV-CANARY", key == "CLOUDFLARE_API_TOKEN" }
	env.UnsetEnv = func(string) error { removed = true; return nil }
	env.RunTokenCommand = func(context.Context, []string, []string) (string, error) { calls++; return "COMMAND-CANARY", nil }
	token, source, _, err := readManagementToken(t.Context(), nil, env, []string{"fake"}, false)
	if err != nil || token != "ENV-CANARY" || !source || !removed || calls != 0 {
		t.Fatalf("env source failed: %v", err)
	}
	env.UnsetEnv = func(string) error { return errors.New("ENV-CANARY") }
	token, _, _, err = readManagementToken(t.Context(), nil, env, nil, true)
	if err == nil || token != "" || strings.Contains(err.Error(), "ENV-CANARY") {
		t.Fatal("unset failure leaked or continued")
	}
	env.LookupEnv = noEnv
	for _, interactive := range []bool{false, true} {
		token, _, _, err = readManagementToken(t.Context(), nil, env, []string{"fake"}, interactive)
		if interactive {
			if err != nil || token != "COMMAND-CANARY" || calls != 1 {
				t.Fatal("interactive command failed")
			}
		} else if err == nil || calls != 0 {
			t.Fatal("unattended command executed")
		}
	}
}

func TestManagementTokenCommandIsBoundedAndStripsCredentialEnvironment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	script := filepath.Join(home, "token-source")
	body := "#!/bin/sh\n[ -z \"$AWS_SECRET_ACCESS_KEY$CLOUDFLARE_API_TOKEN$AGENT_ARCHIVE_PAIR_KEY$OBJECT_ACCESS_KEY\" ] || exit 9\n[ \"$HOME\" = \"" + home + "\" ] || exit 10\nprintf 'COMMAND-CANARY\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, home, time.Now())
	env.Environ = func() []string {
		return []string{"HOME=" + home, "AWS_SECRET_ACCESS_KEY=SECRET", "CLOUDFLARE_API_TOKEN=SECRET", "AGENT_ARCHIVE_PAIR_KEY=SECRET", "OBJECT_ACCESS_KEY=SECRET"}
	}
	token, err := env.runManagementTokenCommand(t.Context(), []string{script})
	if err != nil || strings.TrimSpace(token) != "COMMAND-CANARY" {
		t.Fatalf("command: %v", err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'ERROR-CANARY' >&2\nprintf 'ERROR-CANARY'\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	token, err = env.runManagementTokenCommand(t.Context(), []string{script})
	if err == nil || token != "" || strings.Contains(err.Error(), "ERROR-CANARY") {
		t.Fatal("failed subprocess leaked stdout/stderr")
	}
	var buffer tokenOutput
	_, _ = buffer.Write([]byte(strings.Repeat("x", maxManagementTokenBytes+1)))
	if !buffer.overflow || len(buffer.data) != maxManagementTokenBytes {
		t.Fatal("output allocation unbounded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	token, err = env.runManagementTokenCommand(ctx, []string{script})
	if err == nil || token != "" {
		t.Fatal("canceled command completed")
	}
}

func TestManagementTokenRejectsInvalidBearerHeaderBytes(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"secret\x01canary", "secret\x1bcanary", "secret\x7fcanary", "secreté", "secret:canary", "secret,canary"} {
		if err := validateManagementToken(value); err == nil {
			t.Fatalf("invalid bearer token accepted: %q", value)
		}
	}
	if err := validateManagementToken("abc_DEF-123+/="); err != nil {
		t.Fatal(err)
	}
}

func TestManagementTokenCommandHonorsNoninteractivePolicy(t *testing.T) {
	t.Parallel()
	for _, vars := range []map[string]string{{"CLAUDE_CODE_SESSION_ID": "s"}, {"CODEX_THREAD_ID": "s"}, {"CURSOR_AGENT": "1"}, {envNonInteractive: "1"}} {
		env := withEnvironment(testEnv(t, t.TempDir(), time.Now()), vars)
		env.IsTerminal = func(any) bool { return true }
		env.RunTokenCommand = func(context.Context, []string, []string) (string, error) {
			t.Fatal("noninteractive token command ran")
			return "", nil
		}
		if _, _, _, err := readManagementToken(t.Context(), nil, env, []string{"fake"}, env.interactive(nil)); err == nil {
			t.Fatal("noninteractive acquisition accepted")
		}
	}
}

func TestSetupReviewPreservesCommittedTokenSourceOverStaleDraft(t *testing.T) {
	t.Parallel()
	existing := config.Config{CloudflareTokenCommand: []string{"current-source", "read"}}
	draft := setupDraft{Config: config.Config{CloudflareTokenCommand: []string{"obsolete-source"}}}
	got := reviewedSetupConfig(existing, draft)
	if !reflect.DeepEqual(got.CloudflareTokenCommand, existing.CloudflareTokenCommand) {
		t.Fatal("stale draft replaced current token source")
	}
}
