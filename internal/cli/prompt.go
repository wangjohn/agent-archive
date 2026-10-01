package cli

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/term"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// prompter handles terminal and redirected input without echoing secrets.
type prompter struct {
	in     *bufio.Reader
	out    io.Writer
	source io.Reader
	now    func() time.Time
	style  textStyle
	// spaceAfterAnswer separates interactive setup answers from what follows.
	// Other commands use this prompter too and retain their existing output.
	spaceAfterAnswer bool
	// singleArea is set while setup changes one area of an installed
	// setup, where step headings do not count steps.
	singleArea bool
	// reviewHint, when set, is a line the setup review repeats: where to
	// change what an answer chose for the person.
	reviewHint string
	// createdBuckets are the S3 buckets setup created in this run (see
	// setup_s3_create.go); in memory only.
	createdBuckets []createdS3Bucket
	// guided is a guided R2 bucket creation whose key setup has yet to
	// stage; created is every bucket and key such creations left in the
	// person's Cloudflare account in this run.
	guided  *r2Handoff
	created []*r2Created
}

// step prints a wizard step heading, set apart from the prompts above it.
func (p *prompter) step(n int, title string) {
	heading := fmt.Sprintf("Step %d of 3 · %s", n, title)
	if p.singleArea {
		heading = title
	}
	terminal.Printf(p.out, "\n%s\n\n", p.style.bold(heading))
}

// warn and note print one review item. Continuation lines, such as a link,
// are indented under the item's text.
func (p *prompter) warn(text string, continuation ...string) {
	p.item(p.style.warnMark(), text, continuation)
}

func (p *prompter) note(text string, continuation ...string) {
	p.item(p.style.dim("·"), text, continuation)
}

func (p *prompter) item(mark, text string, continuation []string) {
	terminal.Printf(p.out, "  %s %s\n", mark, text)
	for _, l := range continuation {
		terminal.Println(p.out, "    "+l)
	}
}

// clock returns the prompter's injected clock, or the wall clock when none
// was provided.
func (p *prompter) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out, source: in, style: styleFor(out)}
}

func (p *prompter) line(label string) (string, error) {
	terminal.Print(p.out, label)
	text, err := p.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && text != "" {
			// A final answer with no trailing newline is still a real one.
			if p.spaceAfterAnswer {
				terminal.Println(p.out)
			}
			return strings.TrimSpace(text), nil
		}
		// No more input at all: treated as an error, never as a silent
		// blank. Otherwise a truncated scripted input, or a real Ctrl-D,
		// would make every remaining prompt take its default silently —
		// including "Enable automatic capture?", which defaults to yes —
		// so setup could commit real changes the user never confirmed.
		return "", fmt.Errorf("no more input: %w", err)
	}
	if p.spaceAfterAnswer {
		terminal.Println(p.out)
	}
	return strings.TrimSpace(text), nil
}

// ask writes a prompt and reads its answer. The prompt is question, then
// the choices in brackets with choices[def] the default, then the input
// cursor. A color terminal shows the question in bold when bold is set, the
// default choice in bold, and "›" as the cursor; plain output ends the
// prompt with plainEnd instead, as it always has, so a script reading it
// sees the same text. def outside choices marks no default.
func (p *prompter) ask(question string, bold bool, choices []string, def int, plainEnd string) (string, error) {
	return p.line(p.promptText(question, bold, choices, def, plainEnd))
}

func (p *prompter) promptText(question string, bold bool, choices []string, def int, plainEnd string) string {
	if question == "" && len(choices) == 0 {
		return ""
	}
	if !p.style.color {
		text := question
		if len(choices) > 0 {
			text += " [" + strings.Join(choices, "/") + "]"
		}
		return text + plainEnd
	}
	text := question
	if bold {
		text = p.style.bold(question)
	}
	if len(choices) > 0 {
		styled := make([]string, len(choices))
		for i, c := range choices {
			styled[i] = c
			if i == def {
				styled[i] = p.style.bold(c)
			}
		}
		text += " [" + strings.Join(styled, "/") + "]"
	}
	if text == "" {
		return promptCursor + " "
	}
	return text + " " + promptCursor + " "
}

// promptCursor ends every prompt on a color terminal.
const promptCursor = "›"

// withDefault prompts once with a question, returning def when the answer
// is blank. A blank default shows no bracketed value rather than a
// confusing "[]".
func (p *prompter) withDefault(label, def string) (string, error) {
	return p.defaulted(label, true, def)
}

// choose is withDefault for a menu's answer line, such as "Enter 1-3 [1]":
// the question above it is the bold one, so only the default is.
func (p *prompter) choose(label, def string) (string, error) {
	return p.defaulted(label, false, def)
}

func (p *prompter) defaulted(label string, bold bool, def string) (string, error) {
	var choices []string
	if def != "" {
		choices = []string{def}
	}
	answer, err := p.ask(label, bold, choices, 0, ": ")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

func (p *prompter) yesNo(label string, def bool) (bool, error) {
	choices, defIndex := []string{"Y", "n"}, 0
	if !def {
		choices, defIndex = []string{"y", "N"}, 1
	}
	for {
		answer, err := p.ask(label, true, choices, defIndex, " ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			terminal.Println(p.out, "Please enter y or n.")
		}
	}
}

// heading writes a question that the lines after it answer, such as a
// menu's, in bold. Blank lines leading it stay outside the bold.
func (p *prompter) heading(question string) {
	rest := strings.TrimLeft(question, "\n")
	terminal.Println(p.out, question[:len(question)-len(rest)]+p.style.bold(rest))
}

// option is one numbered entry in a menu. Key is what the caller receives;
// Label is what the user reads.
type option struct {
	Key   string
	Label string
}

// menu prints a question with numbered options and returns the chosen key.
// The user answers with the option's number; a blank answer takes def. The
// option's key, or an unambiguous prefix of it such as y for yes, is also
// accepted, so scripted input keeps working.
func (p *prompter) menu(question, def string, options ...option) (string, error) {
	p.heading(question)
	defNum := ""
	for i, o := range options {
		terminal.Printf(p.out, "  %d) %s\n", i+1, o.Label)
		if o.Key == def {
			defNum = strconv.Itoa(i + 1)
		}
	}
	label := fmt.Sprintf("Enter 1-%d", len(options))
	for {
		answer, err := p.choose(label, defNum)
		if err != nil {
			return "", err
		}
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(options) {
			return options[n-1].Key, nil
		}
		if key, ok := matchOption(answer, options); ok {
			return key, nil
		}
		terminal.Printf(p.out, "Enter a number from 1 to %d.\n", len(options))
	}
}

// matchOption finds the option whose key equals answer, or failing that the
// only option whose key starts with it.
func matchOption(answer string, options []option) (string, bool) {
	answer = strings.ToLower(answer)
	if answer == "" {
		return "", false
	}
	for _, o := range options {
		if answer == o.Key {
			return o.Key, true
		}
	}
	match := ""
	for _, o := range options {
		if strings.HasPrefix(o.Key, answer) {
			if match != "" {
				return "", false
			}
			match = o.Key
		}
	}
	return match, match != ""
}

// retentionDays asks how many days to keep sessions, offering def.
func (p *prompter) retentionDays(def int) (int, error) {
	for {
		answer, err := p.withDefault("Keep sessions for how many days?", strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		value, err := strconv.Atoi(answer)
		if err == nil && value > 0 && value <= 36500 {
			return value, nil
		}
		terminal.Println(p.out, "Enter a number of days between 1 and 36500.")
	}
}

func (p *prompter) secret(label string) (string, error) {
	if file, ok := p.source.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fd := int(file.Fd())
		state, err := term.GetState(fd)
		if err != nil {
			return "", fmt.Errorf("read terminal state: %w", err)
		}
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
		done := make(chan struct{})
		defer func() { signal.Stop(interrupts); close(done); _ = term.Restore(fd, state) }()
		go func() {
			select {
			case sig := <-interrupts:
				_ = term.Restore(fd, state)
				// Exit only after restoring the caller's terminal. A signal
				// must not be mistaken for consent or resume the wizard.
				os.Exit(128 + int(sig.(syscall.Signal)))
			case <-done:
			}
		}()
		terminal.Print(p.out, p.labelText(label))
		value, err := readSecret(fd)
		terminal.Println(p.out)
		if errors.Is(err, io.EOF) {
			// As for a line: no more input is an error, never a blank.
			return "", fmt.Errorf("no more input: %w", err)
		}
		if err != nil {
			return "", fmt.Errorf("cannot hide credential input: %w", err)
		}
		if p.spaceAfterAnswer {
			terminal.Println(p.out)
		}
		return strings.TrimSpace(string(value)), nil
	}
	// Redirected input is read without reproducing its contents. Callers must
	// supply it via a private stream, never a command argument.
	return p.line(p.labelText(label))
}

// labelText styles a prompt written whole, such as "Projects: " or
// "Import 3 sessions? [y/N/edit] ", as ask would: its trailing ": " or " "
// is the plain end, and a bracketed list of choices at its end marks its
// default with a capital letter, as in [y/N].
func (p *prompter) labelText(label string) string {
	if !p.style.color {
		return label
	}
	question := strings.TrimRight(label, " ")
	question = strings.TrimSuffix(question, ":")
	var choices []string
	def := -1
	if strings.HasSuffix(question, "]") {
		if open := strings.LastIndex(question, " ["); open >= 0 {
			choices = strings.Split(question[open+2:len(question)-1], "/")
			question = question[:open]
			for i, c := range choices {
				if c != strings.ToLower(c) {
					def = i
				}
			}
		}
	}
	return p.promptText(question, true, choices, def, "")
}

func (p *prompter) required(label, def string) (string, error) {
	for {
		value, err := p.withDefault(label, def)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
		terminal.Println(p.out, "This value is required.")
	}
}
