package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

type guidedTranscriptFlow string

const (
	guidedFlowPairingSource          guidedTranscriptFlow = "pairing-source"
	guidedFlowPairingReceiver        guidedTranscriptFlow = "pairing-receiver"
	guidedFlowHandoffFile            guidedTranscriptFlow = "handoff-file"
	guidedFlowBackfillEditImport     guidedTranscriptFlow = "backfill-edit-import"
	guidedFlowBackfillUndo           guidedTranscriptFlow = "backfill-undo"
	guidedFlowRecoveryPreviewConfirm guidedTranscriptFlow = "recovery-preview-confirm"
	guidedFlowUninstallLocalPurge    guidedTranscriptFlow = "uninstall-local-purge"
	guidedFlowPurgePlanApply         guidedTranscriptFlow = "purge-plan-apply"
)

// Full command transcripts complement the decision matrix: every transcript
// includes command planning, prompts and committed result or cancellation.
// Sequential KDF work bounds pairing test memory.
func TestGuidedFullCommandTranscripts(t *testing.T) {
	payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: screenNow, ExpiresAt: screenNow.Add(time.Hour), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(payload, code)
	must(t, err)
	for _, flow := range []guidedTranscriptFlow{guidedFlowPairingSource, guidedFlowPairingReceiver, guidedFlowHandoffFile, guidedFlowBackfillEditImport, guidedFlowBackfillUndo, guidedFlowRecoveryPreviewConfirm, guidedFlowUninstallLocalPurge, guidedFlowPurgePlanApply} {
		for _, size := range []struct {
			width  int
			height int
			ascii  bool
		}{{80, 24, false}, {100, 30, false}, {60, 20, false}, {36, 20, true}} {
			for _, color := range []bool{false, true} {
				name := fmt.Sprintf("%s-%dx%d", flow, size.width, size.height)
				if color {
					name += ".color"
				}
				t.Run(name, func(t *testing.T) {
					out := &guidedCommandOutput{caps: promptCapabilities{Color: color, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: !size.ascii, ASCII: size.ascii, Width: size.width, Height: size.height}}
					normalize := func(s string) string { return s }
					run := func(args []string, answers string, env Env) {
						in := &guidedCommandAnswers{input: answers, out: out}
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
					case guidedFlowPairingSource:
						env, home, _ := pairingSourceFixture(t)
						env.Now = func() time.Time { return screenNow }
						env.Clipboard = func([]byte) error { return nil }
						run([]string{"machines", "add", "--name", "laptop"}, "done\n", env)
						userHome, _ := env.UserHomeDir()
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, userHome, "/Users/alex")
							return strings.ReplaceAll(s, home, "/Users/alex/.agent-archive")
						}
					case guidedFlowPairingReceiver:
						f := newScreenFixture(t)
						f.withApps(t, "codex")
						f.env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
						project := f.project(t, "src/项目-long-project")
						run([]string{"setup", "--pair", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}, bundle+"\n"+code+"\nsave\n", f.env)
						normalize = f.normalize
						if strings.Contains(out.String(), bundle) || strings.Contains(out.String(), code) {
							t.Fatal("pairing input leaked")
						}
					case guidedFlowHandoffFile:
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
					case guidedFlowBackfillEditImport:
						f, _ := newImportFixture(t)
						run([]string{"backfill"}, "edit\n120\nyes\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.userHome, "/Users/alex")
							return strings.ReplaceAll(s, f.root, "")
						}
					case guidedFlowBackfillUndo:
						f, _ := newUndoFixture(t)
						run([]string{"backfill", "undo"}, "yes\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.userHome, "/Users/alex")
							return strings.ReplaceAll(s, f.root, "")
						}
					case guidedFlowRecoveryPreviewConfirm:
						env, home, path, id := recoverFixture(t)
						run([]string{"recover", id}, "", env)
						run([]string{"recover", id, "--confirm"}, "", env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, id, "PREVIOUS_ID")
							s = strings.ReplaceAll(s, home, "/Users/alex/.agent-archive")
							return strings.ReplaceAll(s, path, "/Users/alex/src/project/session.jsonl")
						}
					case guidedFlowUninstallLocalPurge:
						f := newScreenFixture(t)
						f.installed(t)
						run([]string{"uninstall", "--delete-local-data"}, "yes\nyes\n", f.env)
						normalize = f.normalize
					case guidedFlowPurgePlanApply:
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
						normalize = func(s string) string {
							return strings.ReplaceAll(f.normalize(s), plan.Digest[:12], "DIGEST")
						}
					}
					text := normalize(out.String())
					text = normalizeGuidedHandoffTranscript(text)
					// Random issued IDs and content hashes are presentation placeholders.
					text = regexp.MustCompile(`[0-9a-f]{64}`).ReplaceAllString(text, "HASH")
					text = regexp.MustCompile(`[0-9a-f]{32}`).ReplaceAllString(text, "MACHINE_ID")
					if color {
						text = strings.ReplaceAll(text, "\x1b", `\e`)
					}
					golden.Check(t, filepath.Join("testdata", "guided-full", name+".txt"), []byte(trimScreenLineEnds(text)))
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
	var errOut bytes.Buffer
	if Run([]string{"machines", "--json"}, nil, out, &errOut, env) != 0 {
		t.Fatalf("JSON failed: %s", out.String())
	}
	var document any
	must(t, json.Unmarshal(out.Bytes(), &document))
	if errOut.Len() != 0 {
		t.Fatalf("JSON stderr: %s", &errOut)
	}
	if !strings.HasPrefix(out.String(), "{") || strings.Contains(out.String(), "? ") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("decorated JSON %q", out.String())
	}
}

// Actual handoff flows retain static, readable output when cursor ownership is
// unavailable. Stdout and stderr are captured independently in the split case.
func TestGuidedFullHandoffOutputSurfaces(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"dumb", "redirected", "stderr-split"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newHandoffFixture(t, false)
			out := &guidedCommandOutput{caps: promptCapabilities{InputTerminal: mode != "redirected", OutputTerminal: mode != "redirected", SharedTerminal: mode == "dumb", ASCII: mode == "dumb", Width: 80, Height: 24}}
			var errOut bytes.Buffer
			f.env.IsTerminal = func(any) bool { return mode != "redirected" }
			path := filepath.Join(t.TempDir(), "handoff.md")
			args := []string{"handoff", f.id}
			answers := "w\n" + path + "\n"
			if mode == "redirected" {
				args = append(args, "--output", path)
				answers = ""
			}
			in := &guidedCommandAnswers{input: answers, out: out}
			out.WriteString("$ agent-archive handoff SESSION\n")
			exit := Run(args, in, out, &errOut, f.env)
			if exit != 0 {
				t.Fatalf("handoff failed: %s\n%s", out.String(), &errOut)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			text := out.String() + "[stderr]\n" + errOut.String() + "[exit 0]\n"
			text = strings.ReplaceAll(text, f.id, "SESSION")
			text = strings.ReplaceAll(text, path, "/Users/alex/handoff.md")
			text = strings.ReplaceAll(text, f.project, "/Users/alex/src/project")
			text = strings.ReplaceAll(text, f.home, "/Users/alex/.agent-archive")
			text = normalizeGuidedHandoffTranscript(text)
			if strings.Contains(text, "\x1b") {
				t.Fatalf("static surface emitted cursor controls: %q", text)
			}
			golden.Check(t, filepath.Join("testdata", "guided-full", "handoff-"+mode+".txt"), []byte(trimScreenLineEnds(text)))
		})
	}
}

func normalizeGuidedHandoffTranscript(text string) string {
	text = regexp.MustCompile(`handoff-[0-9a-f]{8}\.md`).ReplaceAllString(text, "handoff-SESSION.md")
	return regexp.MustCompile(`(handoff: wrote .*?) \([0-9]+ bytes\)`).ReplaceAllString(text, "$1 (BYTES bytes)")
}
