package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

type promptScreen struct {
	bytes.Buffer
	caps promptCapabilities
}

func (s *promptScreen) promptCapabilities() promptCapabilities { return s.caps }

func promptExample() promptModel {
	return promptModel{Question: "Where should your archive live?", Helpers: []string{"Choose storage for sessions from ~/src/長い-project-path. Credentials stay private."}, Primary: []option{{"r2", "Cloudflare R2"}, {"s3", "Amazon S3"}}, Secondary: []actionOption{{"help", "h", "Setup instructions"}}, Default: "s3", Receipt: "Provider"}
}

func TestGuidedPromptSnapshots(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 20}, {36, 20}} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%d-color-%t", size[0], size[1], color), func(t *testing.T) {
				t.Parallel()
				out := &promptScreen{caps: promptCapabilities{Color: color, Width: size[0], Height: size[1]}}
				p := newPrompter(strings.NewReader("\nwork\nsynthetic-secret\n"), out)
				key, err := p.guidedChoice(promptExample())
				must(t, err)
				if key != "s3" {
					t.Fatal(key)
				}
				value, err := p.guidedText(promptModel{Question: "AWS profile", Default: "work", Receipt: "Profile", Label: "Profile"})
				must(t, err)
				if value != "work" {
					t.Fatal(value)
				}
				secret, err := p.guidedText(promptModel{Question: "Secret access key (hidden)", Secret: true, Label: "Credential"})
				must(t, err)
				if secret != "synthetic-secret" || strings.Contains(out.String(), secret) {
					t.Fatal("secret exposed or lost")
				}
				golden.Check(t, filepath.Join("testdata", "prompts", fmt.Sprintf("%dx%d-color-%t.txt", size[0], size[1], color)), []byte(strings.ReplaceAll(trimScreenLineEnds(out.String()), "\x1b", "\\e")))
			})
		}
	}
}

func TestGuidedPromptCollapseRequiresOwnedVisibleRows(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"live", "no-color", "resize", "height", "overflow", "external", "suspend", "typed-ahead", "continued", "redirect", "dumb", "long-echo"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			c := promptCapabilities{Color: true, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 80, Height: 24}
			if mode == "no-color" {
				c.Color = false
			}
			if mode == "redirect" {
				c.InputTerminal = false
			}
			if mode == "dumb" {
				c.ASCII = true
				c.Redraw = false
			}
			out := &promptScreen{caps: c}
			p := newPrompter(strings.NewReader(""), out)
			r := p.renderer()
			region := r.begin(promptExample())
			echo := "2\n"
			switch mode {
			case "resize":
				out.caps.Width = 60
			case "height":
				out.caps.Height = 20
			case "overflow":
				region.rows = 24
			case "external":
				terminal.Println(p.out, "EXTERNAL SENTINEL")
			case "suspend":
				release := p.suspendPrompts()
				terminal.Println(out, "PAGER SENTINEL")
				release()
			case "long-echo":
				echo = strings.Repeat("a", 2000) + "\n"
			}
			r.finish(region, "Provider Amazon S3", echo, false, mode == "typed-ahead", mode == "continued")
			got := out.String()
			collapsed := strings.Contains(got, "\x1b[2K")
			if want := mode == "live" || mode == "no-color"; collapsed != want {
				t.Fatalf("collapse=%t want %t: %q", collapsed, want, got)
			}
			if mode == "dumb" && (!strings.Contains(got, "> Choose [2]: ") || !strings.Contains(got, "OK Provider")) {
				t.Fatal(got)
			}
			if mode == "no-color" && strings.Contains(got, "\x1b[2m") {
				t.Fatal("NO_COLOR added emphasis")
			}
		})
	}
}

func TestGuidedChoicesResolveDefaultsShortcutsAliasesAndStableNumbers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, key, label string }{{"\n", "s3", "Amazon S3"}, {"2\n", "r2", "Cloudflare R2"}, {"13\n", "s3", "Amazon S3"}, {"h\n", "help", "Setup instructions"}, {"s3-existing\n", "s3-existing", "Amazon S3"}, {"wrong\n13\n", "s3", "Amazon S3"}} {
		t.Run(strings.TrimSpace(tc.input), func(t *testing.T) {
			t.Parallel()
			out := &promptScreen{}
			p := newPrompter(strings.NewReader(tc.input), out)
			m := promptExample()
			m.Numbers = []int{2, 13}
			m.Aliases = []option{{"s3-existing", ""}}
			m.ResolveReceipt = storageProviderLabel
			got, err := p.guidedChoice(m)
			must(t, err)
			if got != tc.key || !strings.Contains(out.String(), "✓ Provider "+tc.label) {
				t.Fatalf("key %s output %q", got, out.String())
			}
		})
	}
}

func TestGuidedTextValidatesBeforeReceiptAndSharesBufferedInput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("bad\nwork\nsynthetic-secret\nnext\n"), &out)
	value, err := p.guidedText(promptModel{Question: "Profile", Validate: func(s string) error {
		if s != "work" {
			return errors.New("Choose a configured profile.")
		}
		return nil
	}})
	must(t, err)
	if value != "work" || strings.Contains(out.String(), "✓ Profile bad") {
		t.Fatal(out.String())
	}
	secret, err := p.guidedText(promptModel{Question: "Credential", Secret: true})
	must(t, err)
	next, err := p.line("Legacy: ")
	must(t, err)
	if secret != "synthetic-secret" || next != "next" || strings.Contains(out.String(), secret) {
		t.Fatal("reader lost input or exposed secret")
	}
}

func TestGuidedEOFNeverResolvesDefault(t *testing.T) {
	t.Parallel()
	for _, secret := range []bool{false, true} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(""), &out)
		_, err := p.guidedText(promptModel{Question: "Confirm", Default: "yes", Secret: secret})
		if !errors.Is(err, io.EOF) || strings.Contains(out.String(), symbolOK) {
			t.Fatalf("error %v output %q", err, out.String())
		}
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	_, err := p.guidedChoice(promptExample())
	if !errors.Is(err, io.EOF) || strings.Contains(out.String(), symbolOK) {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestGuidedSecretKeepsBoundedReadersAndSanitizesValidation(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"123456789\n", "bad\nvalid\nnext\n"} {
		out := &promptScreen{}
		p := newPrompter(strings.NewReader(input), out)
		read := func(r *bufio.Reader) (string, error) { return boundedPairingLine(r, 8) }
		value, err := p.guidedText(promptModel{Question: "Pairing input", Default: "forbidden-secret-default", Receipt: "forbidden-secret-receipt", Secret: true, ReadAnswer: read, Validate: func(value string) error {
			if value == "bad" {
				return fmt.Errorf("never show %s", value)
			}
			return nil
		}})
		if strings.Contains(out.String(), "forbidden-secret") || strings.Contains(out.String(), "never show bad") {
			t.Fatal("secret state exposed")
		}
		if strings.HasPrefix(input, "123") {
			if err == nil || !strings.Contains(err.Error(), "size limit") || strings.Contains(out.String(), symbolOK) {
				t.Fatalf("%s %v", out.String(), err)
			}
		} else {
			must(t, err)
			if value != "valid" {
				t.Fatal(value)
			}
			next, err := p.line("")
			must(t, err)
			if next != "next" {
				t.Fatal("bounded reader lost typed ahead")
			}
		}
	}
}

func TestGuidedChoiceCannotDefaultToAnUnavailableAction(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	_, err := p.guidedChoice(promptModel{Question: "Blocked review", Default: "start", Primary: []option{{"check", "Check again"}}})
	if !errors.Is(err, io.EOF) || strings.Contains(out.String(), "✓") || strings.Contains(out.String(), "[start]") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestGuidedWrappedCursorCountsEchoOnlyOnce(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 36, Height: 24}}
	p := newPrompter(strings.NewReader(""), out)
	r := p.renderer()
	region := r.begin(promptModel{Question: "Profile", Label: "Profile", Default: strings.Repeat("d", 60)})
	r.finish(region, "Profile resolved", "\n", false, false, false)
	if want := fmt.Sprintf("\x1b[%dA", region.rows); !strings.Contains(out.String(), want) {
		t.Fatalf("wrapped input label counted twice: %q, want %q", out.String(), want)
	}
}

func TestGuidedTerminalOwnerCanDelegateJobControl(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	p.lineGuard = &promptLineGuard{}
	releaseOuter := p.suspendPrompts(true)
	releaseInner := p.suspendPrompts()
	if !p.lineGuard.delegated.Load() {
		t.Fatal("terminal owner lost job-control authority")
	}
	releaseInner()
	if !p.lineGuard.delegated.Load() {
		t.Fatal("nested release restored prompt authority too early")
	}
	releaseOuter()
	if p.lineGuard.delegated.Load() {
		t.Fatal("prompt authority was not restored")
	}
	p.lineGuard = nil
}
