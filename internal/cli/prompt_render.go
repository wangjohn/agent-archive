package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// promptCapabilities keeps stream ownership separate from color. Synthetic
// writers can supply the same capabilities as a terminal without real stdin.
type promptCapabilities struct {
	Color          bool
	InputTerminal  bool
	OutputTerminal bool
	SharedTerminal bool
	Redraw         bool
	ASCII          bool
	Width          int
	Height         int
}

type promptOutput interface{ promptCapabilities() promptCapabilities }

func capabilitiesFor(in io.Reader, out io.Writer) promptCapabilities {
	out = underlyingWriter(out)
	if synthetic, ok := out.(promptOutput); ok {
		return synthetic.promptCapabilities()
	}
	input, inputOK := in.(*os.File)
	output, outputOK := out.(*os.File)
	c := promptCapabilities{
		Color:          styleFor(out).color,
		InputTerminal:  inputOK && input != nil && term.IsTerminal(int(input.Fd())),
		OutputTerminal: outputOK && output != nil && term.IsTerminal(int(output.Fd())),
		ASCII:          os.Getenv("TERM") == "dumb",
	}
	if c.OutputTerminal {
		c.Width, c.Height, _ = term.GetSize(int(output.Fd()))
	}
	// Separate terminals cannot share an echoed answer's cursor position.
	same := false
	if c.InputTerminal && c.OutputTerminal {
		a, aerr := input.Stat()
		b, berr := output.Stat()
		same = aerr == nil && berr == nil && os.SameFile(a, b)
	}
	c.SharedTerminal = same
	c.Redraw = same && !c.ASCII && c.Width > 0 && c.Height > 0
	return c
}

// promptModel is the presentation contract for guided flows. Keys and aliases
// are inputs; only resolved, visible labels enter receipts. Secret questions
// always use a fixed receipt and never retain an answer in rendering state.
type promptModel struct {
	Question string
	Helpers  []string
	Primary  []option
	// Numbers optionally supplies stable visible indices for a paginated model.
	Numbers        []int
	Validate       func(string) error
	Secondary      []actionOption
	Aliases        []option
	Default        string
	Label          string
	Receipt        string
	ResolveReceipt func(string) string
	Secret         bool
	// ReadAnswer can enforce an existing bound using the shared buffered reader.
	ReadAnswer func(*bufio.Reader) (string, error)
}

// promptWriter observes output through p.out while an answer is being read.
// Subprocesses, pagers, and components writing directly to another stream must
// use suspendPrompts before taking the terminal and release it afterward.
type promptWriter struct {
	w          io.Writer
	generation atomic.Uint64
	mu         sync.Mutex
	trailing   int
}

func (w *promptWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation.Add(1)
	n, err := w.w.Write(b)
	for _, ch := range b[:n] {
		if ch == '\n' {
			w.trailing++
		} else {
			w.trailing = 0
		}
	}
	return n, err
}

func (w *promptWriter) blankLines() int { w.mu.Lock(); defer w.mu.Unlock(); return w.trailing }

// promptRenderer owns only the active block. It never saves entered text.
type promptRenderer struct {
	writer       *promptWriter
	capabilities func() promptCapabilities
	suspended    int
	delegated    int
	epoch        atomic.Uint64
	changes      chan os.Signal
}

type ownedPromptRegion struct {
	caps               promptCapabilities
	rows               int
	cursorColumns      int
	generation         uint64
	epoch              uint64
	valid              bool
	inputAlreadyEchoed bool
}

func (p *prompter) renderer() *promptRenderer {
	if p.render == nil {
		w, ok := p.out.(*promptWriter)
		if !ok {
			w = &promptWriter{w: p.out, trailing: 1}
			p.out = w
		}
		// Retain terminal identity only, never a private redirected reader.
		file, _ := p.source.(*os.File)
		p.render = &promptRenderer{writer: w, capabilities: func() promptCapabilities { return capabilitiesFor(file, w.w) }}
		if file, ok := p.source.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			p.render.changes = make(chan os.Signal, 2)
			signal.Notify(p.render.changes, syscall.SIGWINCH, syscall.SIGCONT)
			if p.lineGuard == nil {
				r := p.render
				p.lineGuard = newPromptLineGuard(int(file.Fd()), func() { r.epoch.Add(1) })
			}
		}
	}
	return p.render
}

// suspendPrompts relinquishes the active region, including across a pager,
// spinner or credential helper. Releasing permits a fresh block, never reuse.
func (p *prompter) suspendPrompts(terminalOwnerHandlesStops ...bool) func() {
	r := p.renderer()
	r.epoch.Add(1)
	r.suspended++
	delegate := len(terminalOwnerHandlesStops) > 0 && terminalOwnerHandlesStops[0]
	if delegate {
		r.delegated++
	}
	if p.lineGuard != nil {
		p.lineGuard.delegated.Store(r.delegated > 0)
	}
	return func() {
		r.epoch.Add(1)
		r.suspended--
		if delegate {
			r.delegated--
		}
		if p.lineGuard != nil {
			p.lineGuard.delegated.Store(r.delegated > 0)
		}
	}
}

// close releases any terminal lifecycle installed by hidden guided input.
func (p *prompter) close() {
	if p.render != nil && p.render.changes != nil {
		signal.Stop(p.render.changes)
	}
	if p.lineGuard != nil {
		p.lineGuard.close()
		p.lineGuard = nil
	}
}

// block centralizes boundaries for migrated guided flows and headings. Legacy
// callers keep their low-level line behavior until explicitly migrated.
func (r *promptRenderer) block(text string) {
	if missing := 2 - r.writer.blankLines(); missing > 0 {
		terminal.Print(r.writer, strings.Repeat("\n", missing))
	}
	terminal.Print(r.writer, text)
}

func (r *promptRenderer) begin(m promptModel) ownedPromptRegion {
	for signalled(r.changes) {
	}
	c := r.capabilities()
	s := textStyle{color: c.Color, width: c.Width}
	cursor := promptCursor
	if c.ASCII {
		cursor = ">"
	}
	var b strings.Builder
	b.WriteString(s.hang("? ", strings.TrimSpace(m.Question)) + "\n")
	if len(m.Helpers) > 0 {
		b.WriteByte('\n')
		for _, h := range m.Helpers {
			b.WriteString(s.hang("  ", h) + "\n")
		}
	}
	if len(m.Primary) > 0 {
		b.WriteByte('\n')
	}
	displayDefault := m.Default
	if m.Secret {
		displayDefault = ""
	}
	for i, o := range m.Primary {
		label := o.Label
		if o.Key == m.Default {
			label += " (default)"
			displayDefault = strconv.Itoa(m.number(i))
		}
		b.WriteString(s.hang(fmt.Sprintf("  %d) ", m.number(i)), label) + "\n")
	}
	if len(m.Secondary) > 0 {
		b.WriteByte('\n')
	}
	for _, o := range m.Secondary {
		key := o.Shortcut
		if o.Key == m.Default {
			key = "Enter"
			displayDefault = ""
		}
		b.WriteString(s.hang("  ["+key+"] ", o.Label) + "\n")
	}
	label := m.Label
	if label == "" {
		label = "Choose"
	}
	if displayDefault != "" {
		label += " [" + displayDefault + "]"
	}
	b.WriteString("\n" + cursor + " " + label + ": ")
	text := b.String()
	r.block(text)
	_, cursorColumns := lineMetrics(text[strings.LastIndex(text, "\n")+1:], c.Width)
	return ownedPromptRegion{caps: c, rows: displayLines(text, c.Width), cursorColumns: cursorColumns, generation: r.writer.generation.Load(), epoch: r.epoch.Load(), valid: c.Redraw && c.InputTerminal && c.OutputTerminal && r.suspended == 0 && !ambiguousPromptWidth(text)}
}

func (r *promptRenderer) finish(region ownedPromptRegion, receipt string, echoed string, secret, typedAhead, interrupted bool) {
	r.resolve(region, symbolOK, receipt, echoed, secret, typedAhead, interrupted)
}

func (r *promptRenderer) resolve(region ownedPromptRegion, mark, receipt, echoed string, secret, typedAhead, interrupted bool) {
	c := r.capabilities()
	rows := region.rows
	if !secret && region.caps.InputTerminal {
		// ReadString includes the newline; it moves the cursor to the next row.
		rows += lineRows(strings.Repeat(" ", region.cursorColumns)+strings.TrimSuffix(echoed, "\n"), region.caps.Width) - lineRows(strings.Repeat(" ", region.cursorColumns), region.caps.Width)
	}
	safe := (secret || strings.HasSuffix(echoed, "\n")) && strings.IndexFunc(strings.TrimSuffix(echoed, "\n"), unicode.IsControl) < 0 && !ambiguousPromptWidth(echoed) && region.valid && c.Redraw && c.Width == region.caps.Width && c.Height == region.caps.Height && rows < c.Height && r.epoch.Load() == region.epoch && r.suspended == 0 && r.writer.generation.Load() == region.generation && !region.inputAlreadyEchoed && !typedAhead && !interrupted
	// Hidden input has no echoed newline; redirected streams have no echo at all.
	if secret || !region.caps.SharedTerminal || region.inputAlreadyEchoed {
		terminal.Println(r.writer)
	}
	if safe {
		terminal.Printf(r.writer, "\x1b[%dA\r", rows)
		for range rows {
			terminal.Print(r.writer, "\x1b[2K\x1b[1B\r")
		}
		terminal.Printf(r.writer, "\x1b[%dA", rows)
	}
	if c.ASCII && mark == symbolOK {
		mark = "OK"
	}
	s := textStyle{color: c.Color, width: c.Width}
	terminal.Println(r.writer, s.dim(s.hang(mark+" ", receiptText(receipt))))
	terminal.Println(r.writer)
}

// beginGuided checks for input that the terminal may have echoed before this
// block. The last buffered answer has no new cursor movement when read, even
// when there is no more pending input afterward.
func (p *prompter) beginGuided(m promptModel) ownedPromptRegion {
	pending := p.inputPending()
	region := p.renderer().begin(m)
	region.inputAlreadyEchoed = pending
	return region
}

// guidedChoice resolves a valid choice before producing its completion receipt.
// Invalid input remains with this field; a retry opens a new owned block.
func (p *prompter) guidedChoice(m promptModel) (string, error) {
	r := p.renderer()
	availableDefault := false
	for _, o := range m.Primary {
		availableDefault = availableDefault || o.Key == m.Default
	}
	for _, o := range m.Secondary {
		availableDefault = availableDefault || o.Key == m.Default
	}
	if !availableDefault {
		m.Default = ""
	}
	for {
		region := p.beginGuided(m)
		raw, interrupted, err := p.guidedRead(m.ReadAnswer)
		if err != nil {
			return "", err
		}
		answer := strings.ToLower(strings.TrimSpace(raw))
		choices := append([]option(nil), m.Primary...)
		for _, o := range m.Secondary {
			choices = append(choices, option{o.Key, o.Label})
		}
		key := ""
		if answer == "" {
			key = m.Default
		} else if n, err := strconv.Atoi(answer); err == nil {
			for i, o := range m.Primary {
				if m.number(i) == n {
					key = o.Key
					break
				}
			}
		} else {
			for _, o := range m.Secondary {
				if answer == strings.ToLower(o.Shortcut) && o.Shortcut != "" {
					key = o.Key
				}
			}
			if key == "" {
				key, _ = matchOption(answer, append(choices, m.Aliases...))
			}
		}
		if key != "" {
			if m.Validate != nil {
				if err := m.Validate(key); err != nil {
					r.resolve(region, symbolWarn, err.Error(), raw, false, p.inputPending(), interrupted)
					continue
				}
			}
			receipt := key
			for _, o := range choices {
				if o.Key == key {
					receipt = o.Label
					break
				}
			}
			if m.ResolveReceipt != nil {
				receipt = m.ResolveReceipt(key)
			}
			// Hidden aliases need a caller-supplied semantic label, never raw input.
			if m.Receipt != "" {
				receipt = m.Receipt + " " + receipt
			}
			r.finish(region, receipt, raw, false, p.inputPending(), interrupted)
			return key, nil
		}
		r.resolve(region, symbolWarn, "Enter one of the available choices.", raw, false, p.inputPending(), interrupted)
	}
}

func (p *prompter) guidedText(m promptModel) (string, error) {
	r := p.renderer()
	for {
		restore, err := p.hideGuidedInput(m.Secret)
		if err != nil {
			return "", err
		}
		region := p.beginGuided(m)
		raw, interrupted, err := p.guidedRead(m.ReadAnswer)
		restore()
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(raw)
		if value == "" && !m.Secret {
			value = m.Default
		}
		if m.Validate != nil {
			if err := m.Validate(value); err != nil {
				message := err.Error()
				if m.Secret {
					message = "Credential was not accepted. Try again."
				}
				echo := raw
				if m.Secret {
					echo = ""
				}
				r.resolve(region, symbolWarn, message, echo, m.Secret, p.inputPending(), interrupted)
				continue
			}
		}
		receipt := m.Receipt
		if m.Secret {
			receipt = "Credential received"
			if value == "" {
				receipt = "Credential skipped"
			}
		} else {
			if receipt == "" {
				receipt = m.Question
			}
			receipt += " " + value
			if m.ResolveReceipt != nil {
				receipt = m.ResolveReceipt(value)
			}
		}
		echo := ""
		if !m.Secret {
			echo = raw
		}
		r.finish(region, receipt, echo, m.Secret, p.inputPending(), interrupted)
		return value, nil
	}
}

func (m promptModel) number(i int) int {
	if len(m.Numbers) == len(m.Primary) {
		return m.Numbers[i]
	}
	return i + 1
}

// Emoji presentation and joined clusters vary between terminals. Their existing
// visible widths still guide formatting, but cannot justify erasing owned rows.
func ambiguousPromptWidth(text string) bool {
	return strings.ContainsAny(text, "\u200d\ufe0f")
}

func receiptText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func (p *prompter) inputPending() bool {
	if p.in.Buffered() > 0 {
		return true
	}
	if f, ok := p.source.(*os.File); ok {
		return terminalInputPending(int(f.Fd()))
	}
	return false
}

// guidedRead shares the same buffered reader as menus, ordinary prompts and
// browser hand-back. Signal notifications only invalidate a region; the caller's
// established interrupt handlers and shell exit statuses remain authoritative.
func (p *prompter) guidedRead(read func(*bufio.Reader) (string, error)) (string, bool, error) {
	changes := p.renderer().changes

	if read == nil {
		read = func(in *bufio.Reader) (string, error) { return in.ReadString('\n') }
	}
	raw, err := read(p.in)
	changed := signalled(changes)
	if err != nil && (!errors.Is(err, io.EOF) || raw == "") {
		if errors.Is(err, io.EOF) {
			return "", true, fmt.Errorf("no more input: %w", err)
		}
		return "", true, fmt.Errorf("read answer: %w", err)
	}
	return raw, changed, nil
}

// hideGuidedInput changes modes before displaying the input cursor, closing the
// race where a fast paste could otherwise arrive while echo is still enabled.
type promptEcho interface{ hidePromptEcho() func() }

func (p *prompter) hideGuidedInput(secret bool) (func(), error) {
	if secret {
		if echo, ok := p.source.(promptEcho); ok {
			return echo.hidePromptEcho(), nil
		}
		if f, ok := p.source.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			if p.lineGuard == nil {
				r := p.renderer()
				p.lineGuard = newPromptLineGuard(int(f.Fd()), func() { r.epoch.Add(1) })
			}
			restore, err := p.lineGuard.hidden()
			if err != nil {
				return nil, fmt.Errorf("cannot hide credential input: %w", err)
			}
			return restore, nil
		}
	}
	return func() {}, nil
}

// noteLineEcho accounts for the terminal's newline without reproducing input.
// Kernel echo and synthetic screen echo bypass the output writer's counters.
func (p *prompter) noteLineEcho(raw string) {
	if w, ok := p.out.(*promptWriter); ok && capabilitiesFor(p.source, w.w).SharedTerminal {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.trailing = 0
		if strings.HasSuffix(raw, "\n") {
			w.trailing = 1
		}
	}
}
