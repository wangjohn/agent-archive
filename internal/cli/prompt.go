package cli

import (
	"bufio"
	"fmt"
	"golang.org/x/term"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// prompter handles terminal and redirected input without echoing secrets.
type prompter struct {
	in     *bufio.Reader
	out    io.Writer
	source io.Reader
	now    func() time.Time
	style  textStyle
}

// textStyle adds ANSI emphasis only when writing to a color terminal, so
// redirected output and tests see plain text.
type textStyle struct{ color bool }

func styleFor(out io.Writer) textStyle {
	file, ok := out.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return textStyle{}
	}
	return textStyle{color: term.IsTerminal(int(file.Fd()))}
}

func (s textStyle) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
func (s textStyle) bold(text string) string   { return s.wrap("1", text) }
func (s textStyle) dim(text string) string    { return s.wrap("2", text) }
func (s textStyle) green(text string) string  { return s.wrap("32", text) }
func (s textStyle) yellow(text string) string { return s.wrap("33", text) }
func (s textStyle) red(text string) string    { return s.wrap("31", text) }

// step prints a wizard step heading, set apart from the prompts above it.
func (p *prompter) step(n int, title string) {
	fmt.Fprintf(p.out, "\n%s\n\n", p.style.bold(fmt.Sprintf("Step %d of 3 · %s", n, title)))
}

// warn and note print one review item. Continuation lines, such as a link,
// are indented under the item's text.
func (p *prompter) warn(text string, continuation ...string) {
	p.item(p.style.yellow("!"), text, continuation)
}
func (p *prompter) note(text string, continuation ...string) {
	p.item(p.style.dim("·"), text, continuation)
}
func (p *prompter) item(mark, text string, continuation []string) {
	fmt.Fprintf(p.out, "  %s %s\n", mark, text)
	for _, l := range continuation {
		fmt.Fprintln(p.out, "    "+l)
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
	fmt.Fprint(p.out, label)
	text, err := p.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && text != "" {
			// A final answer with no trailing newline is still a real one.
			return strings.TrimSpace(text), nil
		}
		// No more input at all: treated as an error, never as a silent
		// blank. Otherwise a truncated scripted input, or a real Ctrl-D,
		// would make every remaining prompt take its default silently —
		// including "Enable automatic capture?", which defaults to yes —
		// so setup could commit real changes the user never confirmed.
		return "", fmt.Errorf("no more input: %w", err)
	}
	return strings.TrimSpace(text), nil
}

// help prints guidance for the prompt that follows: the question flush with
// the prompts, then any continuation lines (a note, or a menu of choices)
// indented beneath it.
func (p *prompter) help(question string, continuation ...string) {
	if question != "" {
		fmt.Fprintln(p.out, question)
	}
	for _, l := range continuation {
		fmt.Fprintln(p.out, "  "+l)
	}
}

// withDefault prompts once, returning def when the answer is blank. A blank
// default shows no bracketed value rather than a confusing "[]".
func (p *prompter) withDefault(label, def string) (string, error) {
	prompt := label + ": "
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]: ", label, def)
	}
	answer, err := p.line(prompt)
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

func (p *prompter) yesNo(label string, def bool) (bool, error) {
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	for {
		answer, err := p.line(fmt.Sprintf("%s [%s] ", label, hint))
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
			fmt.Fprintln(p.out, "Please enter y or n.")
		}
	}
}

// option is one numbered entry in a menu. Key is what the caller receives;
// Label is what the user reads.
type option struct {
	Key, Label string
}

// menu prints a question with numbered options and returns the chosen key.
// The user answers with the option's number; a blank answer takes def. The
// option's key, or an unambiguous prefix of it such as y for yes, is also
// accepted, so scripted input keeps working.
func (p *prompter) menu(question, def string, options ...option) (string, error) {
	fmt.Fprintln(p.out, question)
	defNum := ""
	for i, o := range options {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, o.Label)
		if o.Key == def {
			defNum = strconv.Itoa(i + 1)
		}
	}
	label := fmt.Sprintf("Enter 1-%d", len(options))
	for {
		answer, err := p.withDefault(label, defNum)
		if err != nil {
			return "", err
		}
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(options) {
			return options[n-1].Key, nil
		}
		if key, ok := matchOption(answer, options); ok {
			return key, nil
		}
		fmt.Fprintf(p.out, "Enter a number from 1 to %d.\n", len(options))
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

func (p *prompter) intWithDefault(label string, def int) (int, error) {
	for {
		answer, err := p.withDefault(label, strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		value, err := strconv.Atoi(answer)
		if err == nil && value > 0 && value <= 36500 {
			return value, nil
		}
		fmt.Fprintln(p.out, "Enter a number of days between 1 and 36500.")
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
		fmt.Fprint(p.out, label)
		value, err := term.ReadPassword(fd)
		fmt.Fprintln(p.out)
		if err != nil {
			return "", fmt.Errorf("cannot hide credential input: %w", err)
		}
		return strings.TrimSpace(string(value)), nil
	}
	// Redirected input is read without reproducing its contents. Callers must
	// supply it via a private stream, never a command argument.
	return p.line(label)
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
		fmt.Fprintln(p.out, "This value is required.")
	}
}

// lines reads one answer per call until a blank line ends the list.
func (p *prompter) lines(label string) ([]string, error) {
	fmt.Fprintln(p.out, label)
	var out []string
	for {
		answer, err := p.line("> ")
		if err != nil {
			return nil, err
		}
		if answer == "" {
			return out, nil
		}
		out = append(out, answer)
	}
}
