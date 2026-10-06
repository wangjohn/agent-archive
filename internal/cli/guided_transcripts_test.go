package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Full command transcripts complement the decision matrix: every transcript
// includes command planning, prompts and committed result or cancellation.
// Sequential KDF work bounds pairing test memory.
func TestGuidedFullCommandTranscripts(t *testing.T) {
	payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: screenNow, ExpiresAt: screenNow.Add(time.Hour), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(payload, code)
	must(t, err)
	for _, flow := range []string{"pairing-source", "pairing-receiver", "handoff-file", "backfill-edit-import", "uninstall-local-purge", "purge-plan-apply"} {
		for _, size := range []struct {
			width, height int
			ascii         bool
		}{{80, 24, false}, {100, 30, false}, {60, 20, false}, {36, 20, true}} {
			for _, color := range []bool{false, true} {
				name := fmt.Sprintf("%s-%dx%d", flow, size.width, size.height)
				if color {
					name += ".color"
				}
				t.Run(name, func(t *testing.T) {
					out := &guidedCommandOutput{caps: promptCapabilities{Color: color, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, ASCII: size.ascii, Width: size.width, Height: size.height}}
					normalize := func(s string) string { return s }
					run := func(args []string, answers string, env Env) {
						in := &guidedAnswers{input: answers, out: out}
						env.IsTerminal = func(any) bool { return true }
						terminalCommand := fmt.Sprintf("$ agent-archive %s\n", strings.Join(args, " "))
						out.WriteString(terminalCommand)
						exit := Run(args, in, out, out, env)
						fmt.Fprintf(&out.Buffer, "[exit %d]\n", exit)
						if exit != 0 {
							t.Fatalf("command failed:\n%s", out.String())
						}
					}
					switch flow {
					case "pairing-source":
						env, home, _ := pairingSourceFixture(t)
						env.Now = func() time.Time { return screenNow }
						env.Clipboard = func([]byte) error { return nil }
						run([]string{"machines", "add", "--name", "laptop"}, "done\n", env)
						userHome, _ := env.UserHomeDir()
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, userHome, "/Users/alex")
							return strings.ReplaceAll(s, home, "/Users/alex/.agent-archive")
						}
					case "pairing-receiver":
						f := newScreenFixture(t)
						f.withApps(t, "codex")
						f.env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
						project := f.project(t, "src/项目-long-project")
						run([]string{"setup", "--pair", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}, bundle+"\n"+code+"\nsave\n", f.env)
						normalize = f.normalize
						if strings.Contains(out.String(), bundle) || strings.Contains(out.String(), code) {
							t.Fatal("pairing input leaked")
						}
					case "handoff-file":
						f := newHandoffFixture(t, false)
						path := filepath.Join(t.TempDir(), "handoff.md")
						run([]string{"handoff", f.id}, "w\n"+path+"\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.id, "SESSION")
							s = strings.ReplaceAll(s, path, "/Users/alex/handoff.md")
							s = strings.ReplaceAll(s, f.project, "/Users/alex/src/项目-long-project")
							return strings.ReplaceAll(s, f.home, "/Users/alex/.agent-archive")
						}
						if _, err := os.Stat(path); err != nil {
							t.Fatal(err)
						}
					case "backfill-edit-import":
						f, _ := newImportFixture(t)
						run([]string{"backfill"}, "edit\n120\nyes\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.userHome, "/Users/alex")
							return strings.ReplaceAll(s, f.root, "")
						}
					case "uninstall-local-purge":
						f := newScreenFixture(t)
						f.installed(t)
						run([]string{"uninstall", "--purge"}, "yes\nyes\n", f.env)
						normalize = f.normalize
					case "purge-plan-apply":
						f := newScreenFixture(t)
						f.installed(t)
						// Synthetic orphan content is never an archived developer transcript.
						must(t, f.bucket.Put(context.Background(), "sessions/codex/"+strings.Repeat("a", 32)+"/source."+strings.Repeat("b", 64)+".jsonl.gz", []byte("synthetic orphan")))
						run([]string{"purge", "plan"}, "", f.env)
						var path string
						for line := range strings.SplitSeq(out.String(), "\n") {
							if value, ok := strings.CutPrefix(line, "Plan: "); ok {
								path = value
							}
						}
						var plan purge.Plan
						must(t, jsonFile(path, &plan))
						_, err := config.SetPaused(f.home, true)
						must(t, err)
						run([]string{"purge", "apply", path}, plan.Digest[:12]+"\n", f.env)
						normalize = f.normalize
					}
					text := normalize(out.String())
					text = regexp.MustCompile(`(handoff: wrote .*?) \([0-9]+ bytes\)`).ReplaceAllString(text, "$1 (BYTES bytes)")
					// Random issued IDs and content hashes are presentation placeholders.
					text = regexp.MustCompile(`[0-9a-f]{64}`).ReplaceAllString(text, "HASH")
					text = regexp.MustCompile(`[0-9a-f]{32}`).ReplaceAllString(text, "MACHINE_ID")
					if color {
						text = strings.ReplaceAll(text, "\x1b", `\e`)
					}
					golden.Check(t, filepath.Join("testdata", "guided-full", name+".txt"), []byte(text))
				})
			}
		}
	}
}

// A redirected structured result remains a JSON document despite human guided
// presentation changes elsewhere. Provider checks and secrets are not invoked.
func TestGuidedMachinesJSONHasNoPromptDecoration(t *testing.T) {
	t.Parallel()
	env, _, _ := pairingSourceFixture(t)
	out := &guidedCommandOutput{}
	if Run([]string{"machines", "--json"}, nil, out, out, env) != 0 {
		t.Fatalf("JSON failed: %s", out.String())
	}
	if !strings.HasPrefix(out.String(), "{") || strings.Contains(out.String(), "? ") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("decorated JSON %q", out.String())
	}
}
