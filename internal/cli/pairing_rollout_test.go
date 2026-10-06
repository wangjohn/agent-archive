package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestFirstSetupOffersPairingWithoutFlags(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.IsTerminal = func(any) bool { return true }
	var output bytes.Buffer
	opts, input, err := firstSetupPairingQuestion(setupOptions{}, strings.NewReader("yes\nbundle\n"), &output, env)
	if err != nil || !opts.pair || !strings.Contains(output.String(), "Already set up on another machine?") {
		t.Fatalf("pairing offer: %+v %v %s", opts, err, &output)
	}
	remaining, err := io.ReadAll(input)
	if err != nil || string(remaining) != "bundle\n" {
		t.Fatalf("pairing input lost: %q %v", remaining, err)
	}
}

func TestFirstSetupPairingSkipsNonFreshAndNoninteractiveInvocations(t *testing.T) {
	for _, name := range []string{"scripted", "explicit-pair", "pair-file", "existing", "draft", "agent", "redirected-input", "redirected-output"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := testEnv(t, home, time.Now())
			input := &unreadInput{}
			var out bytes.Buffer
			env.IsTerminal = func(stream any) bool {
				if name == "redirected-input" && stream == input {
					return false
				}
				if name == "redirected-output" && stream == &out {
					return false
				}
				return true
			}
			opts := setupOptions{}
			switch name {
			case "scripted":
				opts.yes = true
			case "explicit-pair":
				opts.pair = true
			case "pair-file":
				opts.pairFile = "bundle"
			case "existing":
				must(t, config.Save(home, config.Config{}))
			case "draft":
				must(t, local.Write(draftPath(home), setupDraft{Version: draftFormat, Step: 1}))
			case "agent":
				env.LookupEnv = func(key string) (string, bool) { return "session", key == "CODEX_THREAD_ID" }
			}
			got, sameInput, err := firstSetupPairingQuestion(opts, input, &out, env)
			if err != nil || got.pair != opts.pair || sameInput != input || out.Len() != 0 {
				t.Fatalf("unexpected pairing question: %+v %v %s", got, err, &out)
			}
		})
	}
}

func TestFreshSetupPairingRedirectReleasesSetupLock(t *testing.T) {
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.IsTerminal = func(any) bool { return true }
	var output bytes.Buffer
	err := setup(strings.NewReader("yes\nbundle\n"), &output, &output, env, false, skillsUnchanged, false)
	var request *setupPairingRequest
	if !errors.As(err, &request) {
		t.Fatalf("expected pairing redirect: %v %s", err, &output)
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		t.Fatalf("pairing cannot reacquire setup lock: %v", err)
	}
	defer release()
	remaining, err := io.ReadAll(request.input)
	if err != nil || string(remaining) != "bundle\n" {
		t.Fatalf("pairing input lost: %q %v", remaining, err)
	}
}

func TestFirstRunPairingCommitsUsingOriginalTerminalAndBufferedAnswers(t *testing.T) {
	now := time.Now().UTC()
	payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(payload, code)
	must(t, err)
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	store := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
	input := strings.NewReader("yes\n" + bundle + "\nall-projects\nno\nsave\n")
	var output bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == input || stream == &output }
	env.PairingTerminal = func() (io.ReadWriteCloser, error) {
		t.Fatal("lost the original terminal identity")
		return nil, errors.New("unexpected terminal")
	}
	env.PairingCode = func() (string, error) { return code, nil }
	if exit := Run([]string{"setup"}, input, &output, &output, env); exit != 0 {
		t.Fatalf("first-run pairing exit %d: %s", exit, &output)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.MachineName != "laptop" || cfg.MachineAssignment == nil || cfg.MachineAssignment.PairingID != payload.PairingID {
		t.Fatalf("pairing not committed: %+v %v %s", cfg, err, &output)
	}
}
