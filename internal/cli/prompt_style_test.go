package cli

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// styledPrompter reads answers from input and writes to out in style.
func styledPrompter(input string, out *bytes.Buffer, style textStyle) *prompter {
	return &prompter{in: bufio.NewReader(strings.NewReader(input)), out: out, source: strings.NewReader(input), style: style}
}

// askAll puts the same questions to p that setup does, one of each kind,
// and returns the answers as one string.
func askAll(t *testing.T, p *prompter) string {
	t.Helper()
	yes, err := p.yesNo("Include Codex?", true)
	must(t, err)
	no, err := p.yesNo("Include Cursor?", false)
	must(t, err)
	profile, err := p.withDefault("AWS profile", "work")
	must(t, err)
	byNumber, err := p.menu("Where should sessions be stored?", "r2", option{"r2", "Cloudflare R2"}, option{"s3", "Amazon S3"})
	must(t, err)
	byKey, err := p.menu("What next?", "fix", option{"fix", "Fix it"}, option{"retry", "Retry the check"})
	must(t, err)
	days, err := p.retentionDays(90)
	must(t, err)
	path, err := p.line(p.labelText("Project path: "))
	must(t, err)
	return strings.Join([]string{boolWord(yes), boolWord(no), profile, byNumber, byKey, strconv.Itoa(days), path}, " ")
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Piped input, and a terminal with NO_COLOR, see the prompts as they were
// before color: no escape codes and no › cursor, so a script that reads the
// prompts, or answers them, keeps working.
func TestPlainPromptsAreUnchanged(t *testing.T) {
	t.Parallel()
	noColor := terminalStyle(func(key string) string {
		return map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color"}[key]
	}, 80)
	if noColor.color {
		t.Fatal("NO_COLOR left color on")
	}
	for name, style := range map[string]textStyle{"piped": {}, "NO_COLOR": noColor} {
		var out bytes.Buffer
		p := styledPrompter("y\nn\nadmin\n2\nretry\n30\n~/src/app\n", &out, style)
		if got, want := askAll(t, p), "yes no admin s3 retry 30 ~/src/app"; got != want {
			t.Errorf("%s: answers %q, want %q", name, got, want)
		}
		want := "Include Codex? [Y/n] Include Cursor? [y/N] AWS profile [work]: " +
			"Where should sessions be stored?\n  1) Cloudflare R2\n  2) Amazon S3\nEnter 1-2 [1]: " +
			"What next?\n  1) Fix it\n  2) Retry the check\nEnter 1-2 [1]: " +
			"Keep sessions for how many days? [90]: Project path: "
		if out.String() != want {
			t.Errorf("%s: wrote\n%q\nwant\n%q", name, out.String(), want)
		}
	}
}

// A color terminal shows each question in bold, its default in bold, and
// the › cursor, and reads the same answers, blank ones included.
func TestColorPromptsBoldTheQuestionAndDefault(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := styledPrompter("\nyes\n\n\n1\n\n~/src/app\n", &out, textStyle{color: true})
	if got, want := askAll(t, p), "yes yes work r2 fix 90 ~/src/app"; got != want {
		t.Errorf("answers %q, want %q", got, want)
	}
	written := out.String()
	for _, want := range []string{
		"\x1b[1mInclude Codex?\x1b[0m [\x1b[1mY\x1b[0m/n] › ",
		"\x1b[1mInclude Cursor?\x1b[0m [y/\x1b[1mN\x1b[0m] › ",
		"\x1b[1mAWS profile\x1b[0m [\x1b[1mwork\x1b[0m] › ",
		"\x1b[1mWhere should sessions be stored?\x1b[0m\n",
		// A menu's answer line is not a question: only its default is bold.
		"\nEnter 1-2 [\x1b[1m1\x1b[0m] › ",
		"\x1b[1mKeep sessions for how many days?\x1b[0m [\x1b[1m90\x1b[0m] › ",
		"\x1b[1mProject path\x1b[0m › ",
	} {
		if !strings.Contains(written, want) {
			t.Errorf("wrote\n%q\nwant it to contain %q", written, want)
		}
	}
	if strings.Contains(written, ": ") {
		t.Errorf("wrote %q, want every prompt to end with the › cursor", written)
	}
}

// A prompt written whole keeps its choices, and the capital one is the
// default.
func TestLabelTextStylesAWholePrompt(t *testing.T) {
	t.Parallel()
	p := styledPrompter("", &bytes.Buffer{}, textStyle{color: true})
	got := p.labelText("Import 3 sessions from 1 project? [y/N/edit] ")
	want := "\x1b[1mImport 3 sessions from 1 project?\x1b[0m [y/\x1b[1mN\x1b[0m/edit] › "
	if got != want {
		t.Errorf("labelText = %q, want %q", got, want)
	}
	plain := styledPrompter("", &bytes.Buffer{}, textStyle{})
	if got := plain.labelText("Import 3 sessions? [y/N/edit] "); got != "Import 3 sessions? [y/N/edit] " {
		t.Errorf("plain labelText = %q", got)
	}
	if got := p.labelText(""); got != "" {
		t.Errorf("an empty label wrote %q", got)
	}
}

// A menu's question that starts with a blank line keeps the blank line
// outside the bold.
func TestMenuHeadingKeepsLeadingBlankLinesPlain(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := styledPrompter("1\n", &out, textStyle{color: true})
	_, err := p.menu("\nWhat would you like to change?", "back", option{"back", "Nothing"})
	must(t, err)
	if !strings.HasPrefix(out.String(), "\n\x1b[1mWhat would you like to change?\x1b[0m\n") {
		t.Errorf("wrote %q", out.String())
	}
}

// storageCheckEnv opens store, or fails with openErr.
func storageCheckEnv(store storage.ObjectStore, openErr error) Env {
	return Env{
		OpenStore: func(config.Config) (storage.ObjectStore, error) { return store, openErr },
		Now:       func() time.Time { return screenNow },
	}
}

// On a terminal that redraws, the check's line resolves in place: the
// spinner is cleared and ✓ takes its line, with only the ✓ colored.
func TestStorageCheckSpinnerResolvesToCheckMark(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := styledPrompter("", &out, textStyle{color: true, live: true})
	cfg := config.Config{}
	if err := runStorageCheck(p, &cfg, storageCheckEnv(storagetest.NewMemoryStore(), nil)); err != nil {
		t.Fatal(err)
	}
	written := out.String()
	if !strings.HasSuffix(written, "\r\x1b[K\x1b[32m✓\x1b[0m Connected to your storage.\n") {
		t.Errorf("wrote %q, want the spinner's line cleared, then ✓", written)
	}
	if strings.Count(written, "\n") != 1 {
		t.Errorf("wrote %q, want the check on one line", written)
	}
	if !cfg.StorageVerifiedAt.Equal(screenNow) {
		t.Errorf("verified at %v, want the check recorded", cfg.StorageVerifiedAt)
	}
}

// A failed check stops the spinner and clears its line before returning,
// so the diagnosis's ✗ headline takes that line and nothing redraws over
// it.
func TestStorageCheckSpinnerStopsOnError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := styledPrompter("", &out, textStyle{color: true, live: true})
	cfg := config.Config{}
	err := runStorageCheck(p, &cfg, storageCheckEnv(nil, errors.New("no credentials")))
	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("err = %v", err)
	}
	written := out.String()
	if !strings.HasSuffix(written, "\r\x1b[K") || strings.Contains(written, "\n") {
		t.Errorf("wrote %q, want only the spinner, its line cleared", written)
	}
	time.Sleep(2 * spinnerInterval)
	if out.String() != written {
		t.Errorf("the spinner wrote %q after the check failed", strings.TrimPrefix(out.String(), written))
	}
}

// Without a terminal there is no spinner: the line is written plainly, and
// a failure leaves a blank line before its diagnosis.
func TestStorageCheckWithoutATerminalPrintsPlainLines(t *testing.T) {
	t.Parallel()
	var ok bytes.Buffer
	cfg := config.Config{}
	must(t, runStorageCheck(styledPrompter("", &ok, textStyle{}), &cfg, storageCheckEnv(storagetest.NewMemoryStore(), nil)))
	if got, want := ok.String(), "Checking your storage connection…\n✓ Connected to your storage.\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
	var failed bytes.Buffer
	if err := runStorageCheck(styledPrompter("", &failed, textStyle{}), &cfg, storageCheckEnv(nil, errors.New("no credentials"))); err == nil {
		t.Fatal("the check passed")
	}
	if got, want := failed.String(), "Checking your storage connection…\n\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

// Ctrl-C during the check stops the spinner, clears its line, says so, and
// returns at once, leaving the configuration as it was even though the
// check is still running.
func TestStorageCheckStopsCleanlyOnInterrupt(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := styledPrompter("", &out, textStyle{color: true, live: true})
	signals := make(chan os.Signal, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stopped := make(chan struct{})
	env := Env{
		OpenStore: func(config.Config) (storage.ObjectStore, error) {
			signals <- os.Interrupt
			<-release
			return storagetest.NewMemoryStore(), nil
		},
		Now: func() time.Time { return screenNow },
		Interrupts: func() (<-chan os.Signal, func()) {
			return signals, func() { close(stopped) }
		},
	}
	cfg := config.Config{}
	err := runStorageCheck(p, &cfg, env)
	if !errors.Is(err, errStorageCheckInterrupted) {
		t.Fatalf("err = %v, want the check interrupted", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("the check kept listening for interrupts after it returned")
	}
	if got, want := out.String(), "\r\x1b[K\x1b[31m✗\x1b[0m Stopped checking your storage connection.\n"; !strings.HasSuffix(got, want) {
		t.Errorf("wrote %q, want the spinner's line cleared, then %q", got, want)
	}
	if !cfg.StorageVerifiedAt.IsZero() {
		t.Error("an interrupted check recorded a verification")
	}
}
