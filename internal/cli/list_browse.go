package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// browseInteractive reports whether list/show should offer a session picker:
// both stdin and stdout are terminals, and the caller is not forcing JSON.
// Interactive list skips the pager so the prompt stays with the table.
func browseInteractive(env sessionBrowseDependencies, stdin io.Reader, stdout io.Writer) bool {
	return env.isTerminal(stdin) && env.isTerminal(stdout)
}

// browseAction is what the session browser does after a prompt.
type browseAction int

const (
	// browseBack returns to the list.
	browseBack browseAction = iota
	// browseQuit leaves the browser.
	browseQuit
	// browseRedraw draws the session's details again.
	browseRedraw
	// browseStay asks again without redrawing, keeping a message visible.
	browseStay
)

// sessionBrowser is interactive list and bare show: pick a row, read its
// summary, open its transcript with t, go back to the list, or quit. The
// list and the details replace each other on the terminal's alternate
// screen (altScreen), each cut to fit the window. Transcript content is
// shown only when t is pressed.
type sessionBrowser struct {
	env     sessionBrowserDependencies
	prompt  *prompter
	stdout  io.Writer
	stderr  io.Writer
	store   storage.ObjectStore
	format  listFormatOptions
	noPager bool
	screen  *altScreen
	// list is the session table, which keeps its page between visits.
	list *sessionPicker
	// last is the session whose details were shown last, printed to the
	// normal screen on the way out so its ID stays in scrollback.
	last *sessionView
}

// runSessionBrowser browses sessions until the user quits (q, an empty
// answer at the list, or end of input), then prints the last session viewed.
func runSessionBrowser(env sessionBrowserDependencies, p *prompter, stdout, stderr io.Writer, store storage.ObjectStore, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, noPager bool, command string) int {
	b := &sessionBrowser{env: env, prompt: p, stdout: stdout, stderr: stderr, store: store, format: format, noPager: noPager, screen: enterAltScreen(stdout, env)}
	b.list = &sessionPicker{env: env, clear: b.screen.clear}
	// Deferred as well, so not even a panic leaves the terminal on the
	// alternate screen; leave does nothing the second time.
	defer b.screen.leave()
	err := b.run(sessions, totalMatched, truncated)
	b.screen.leave()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
		return 1
	}
	if b.last != nil {
		renderSessionSummary(stdout, *b.last, b.summaryOptions(true))
	}
	return 0
}

func (b *sessionBrowser) run(sessions []archive.Metadata, totalMatched int, truncated bool) error {
	for {
		b.screen.clear()
		row, ok, err := b.list.pick(b.prompt, b.stdout, sessions, totalMatched, truncated, b.format, "show")
		if err != nil || !ok {
			return err
		}
		stop := startActivity(b.stdout, "Loading session…")
		view, err := readSessionView(context.Background(), b.store, row.HarnessKey, row.SessionID)
		stop()
		if err != nil {
			return err
		}
		b.last = &view
		action, err := b.details(view, row)
		if err != nil || action == browseQuit {
			return err
		}
	}
}

// details shows one session's summary and reads what to do next.
func (b *sessionBrowser) details(view sessionView, row listRow) (browseAction, error) {
	for {
		b.screen.clear()
		var summary bytes.Buffer
		renderSessionSummary(&summary, view, b.summaryOptions(false))
		hint := b.transcriptHint()
		cut := b.drawDetails(summary.String(), hint)
		action, err := b.detailsPrompt(row, summary.Bytes(), hint, cut)
		if err != nil || action != browseRedraw {
			return action, err
		}
	}
}

// minDetailLines is the fewest summary lines the details show, however
// short the terminal.
const minDetailLines = 5

// detailsQuestion is the details' prompt; [m] is offered when the summary
// was cut to fit.
func detailsQuestion(cut bool) string {
	if cut {
		return "[t] transcript  [m] more  [Enter/b] back to list  [q] quit"
	}
	return "[t] transcript  [Enter/b] back to list  [q] quit"
}

// drawDetails prints the summary. When it does not fit the terminal above
// the hint and the prompt, only the lines that fit are printed, then how
// many more there are; cut reports that.
func (b *sessionBrowser) drawDetails(summary, hint string) (cut bool) {
	width, height, ok := b.env.terminalSize(b.stdout)
	if !ok {
		terminal.Print(b.stdout, summary)
		return false
	}
	// Below the summary: a blank line, the hint, and the prompt.
	chrome := 1 + displayLines(hint, width) + displayLines(b.prompt.promptText(detailsQuestion(true), false, nil, -1, ": "), width)
	if displayLines(summary, width)+chrome <= height {
		terminal.Print(b.stdout, summary)
		return false
	}
	lines := strings.Split(strings.TrimSuffix(summary, "\n"), "\n")
	// One row is kept for the line saying how many more there are.
	budget := max(height-chrome-1, minDetailLines)
	shown, used := 0, 0
	for shown < len(lines) && used+lineRows(lines[shown], width) <= budget {
		used += lineRows(lines[shown], width)
		shown++
	}
	if shown == len(lines) {
		terminal.Print(b.stdout, summary)
		return false
	}
	shown = max(shown, 1)
	for _, line := range lines[:shown] {
		terminal.Println(b.stdout, line)
	}
	more := len(lines) - shown
	noun := "lines"
	if more == 1 {
		noun = "line"
	}
	terminal.Println(b.stdout, b.format.Style.dim(fmt.Sprintf("… %d more %s", more, noun)))
	return true
}

// transcriptHint says how to use and leave the pager t opens, or is empty
// when the transcript is printed without one.
func (b *sessionBrowser) transcriptHint() string {
	command, page := resolvePagerCommand(b.env, b.noPager, b.stdout)
	if !page {
		return ""
	}
	if isLess(command) {
		return "t opens the transcript: scroll with the wheel or arrows, q returns here."
	}
	return "t opens the transcript in your pager; quit it to return here."
}

func (b *sessionBrowser) detailsPrompt(row listRow, summary []byte, hint string, cut bool) (browseAction, error) {
	for first := true; ; first = false {
		terminal.Println(b.stdout)
		if first && hint != "" {
			terminal.Println(b.stdout, b.format.Style.dim(hint))
		}
		answer, err := b.prompt.line(b.prompt.promptText(detailsQuestion(cut), false, nil, -1, ": "))
		if err != nil {
			return endOfInput(err)
		}
		switch strings.ToLower(answer) {
		case "", "b", "back":
			return browseBack, nil
		case "q", "quit":
			return browseQuit, nil
		case "t", "transcript":
			action, err := b.transcript(row)
			if err != nil || action != browseStay {
				return action, err
			}
		case "m", "more":
			// The whole summary, through the pager as the transcript is.
			return b.page(summary)
		default:
			if cut {
				terminal.Println(b.stdout, "Enter t for the transcript, m for the whole summary, b (or just Enter) for the list, or q to quit.")
			} else {
				terminal.Println(b.stdout, "Enter t for the transcript, b (or just Enter) for the list, or q to quit.")
			}
		}
	}
}

// transcript downloads and verifies the session's bundle and shows its
// transcript through the pager. A bundle that cannot be read is reported
// under the details, which stay open.
func (b *sessionBrowser) transcript(row listRow) (browseAction, error) {
	ctx := context.Background()
	stop := startActivity(b.stdout, "Loading transcript…")
	text, err := b.renderTranscript(ctx, row)
	stop()
	if err != nil {
		terminal.Printf(b.stderr, "agent-archive: show: %s\n", describeBundleError(err, row.SessionID, "--transcript"))
		return browseStay, nil
	}
	return b.page(text)
}

// page shows text through the pager on a cleared screen and returns to the
// details once the user is done with it. A signal while the pager runs
// stops the pager, then the browser restores the screen and exits.
func (b *sessionBrowser) page(text []byte) (browseAction, error) {
	b.screen.clear()
	restoreTerminal := saveTerminalState(b.prompt.source)
	pagerCtx, stopPager := context.WithCancel(context.Background())
	b.screen.startPaging(stopPager)
	paged, waited, err := pageText(pagerCtx, b.stdout, b.stderr, b.env, b.noPager, true, text)
	sig := b.screen.endPaging()
	stopPager()
	if sig != nil {
		// A signal stopped the pager; the pager has exited, so exit as the
		// signal would have. A pager killed before it could restore the
		// terminal's modes (one behind a pipe) leaves them raw, so they
		// are restored first.
		restoreTerminal()
		b.screen.exitForSignal(sig)
		return browseQuit, nil
	}
	if err != nil {
		return browseQuit, err
	}
	if paged && waited {
		b.screen.reenter()
		return browseRedraw, nil
	}
	// Printed without a pager, or by a pager that may have returned at
	// once: wait, so the text stays on screen until the user asks for the
	// details again.
	action, err := b.transcriptPrompt()
	if paged {
		b.screen.reenter()
	}
	return action, err
}

func (b *sessionBrowser) transcriptPrompt() (browseAction, error) {
	for {
		terminal.Println(b.stdout)
		answer, err := b.prompt.line(b.prompt.promptText("[Enter/b] back to details  [q] quit", false, nil, -1, ": "))
		if err != nil {
			return endOfInput(err)
		}
		switch strings.ToLower(answer) {
		case "", "b", "back":
			return browseRedraw, nil
		case "q", "quit":
			return browseQuit, nil
		default:
			terminal.Println(b.stdout, "Enter b (or just Enter) for the details, or q to quit.")
		}
	}
}

func (b *sessionBrowser) renderTranscript(ctx context.Context, row listRow) ([]byte, error) {
	key, err := locateMetadataKey(ctx, b.store, row.HarnessKey, row.SessionID)
	if err != nil {
		return nil, err
	}
	view, bundle, err := loadVerifiedSession(ctx, b.store, key)
	if err != nil {
		return nil, err
	}
	t, err := buildTranscript(bundle)
	if err != nil {
		return nil, fmt.Errorf("normalized view unavailable: %w", err)
	}
	var buf bytes.Buffer
	renderTranscript(&buf, view, t, transcriptOptions{summaryOptions: b.summaryOptions(false)})
	return buf.Bytes(), nil
}

func (b *sessionBrowser) summaryOptions(hints bool) summaryOptions {
	return summaryOptions{Now: b.format.Now, Style: b.format.Style, Projects: b.format.Projects, Hints: hints}
}

// endOfInput turns the end of input (Ctrl-D, or a script's last line) into
// quitting; any other read error is returned.
func endOfInput(err error) (browseAction, error) {
	if errors.Is(err, io.EOF) {
		return browseQuit, nil
	}
	return browseQuit, err
}

// pickBrowseSession prints the numbered table and prompts until the user
// selects a row (ok=true), quits or input ends (ok=false), or an error
// occurs. A table taller than the terminal is shown a page at a time.
func pickBrowseSession(env terminalSizeDependencies, p *prompter, stdout io.Writer, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, action string) (row listRow, ok bool, err error) {
	list := &sessionPicker{env: env}
	return list.pick(p, stdout, sessions, totalMatched, truncated, format, action)
}

// minPickerPageRows is the fewest rows a page of the picker shows, however
// short the terminal.
const minPickerPageRows = 3

// sessionPicker is the numbered session table with its prompt. When the
// table, its footer, and the prompt do not fit the terminal's height, it
// shows one page of rows at a time, with n and p to move between pages.
// Row numbers stay those of the whole table, so any listed number or short
// ID can be typed from any page.
type sessionPicker struct {
	env terminalSizeDependencies
	// clear, when set, blanks the screen before another page is drawn.
	clear func()
	// start is the position, in the table's top-to-bottom order, of the
	// first row of the page shown. It survives returning to the list.
	start int
}

// pickerPage is the rows [start, end) of the table, in its top-to-bottom
// order.
type pickerPage struct{ start, end int }

func (l *sessionPicker) pick(p *prompter, stdout io.Writer, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, action string) (listRow, bool, error) {
	format.Numbered = true
	rows := formatSessionRows(sessions, format)
	groups := sessionTableGroups(rows, format)
	question := p.promptText("Enter number (or unique short SESSION_ID) to "+action+", or q to quit", true, nil, -1, ": ")
	var footer bytes.Buffer
	printListFooter(&footer, len(sessions), totalMatched, truncated)
	for {
		pages := l.pages(stdout, groups, format, len(rows), footer.String(), question)
		page := 0
		for i, pg := range pages {
			if l.start >= pg.start && l.start < pg.end {
				page = i
			}
		}
		paged := len(pages) > 1
		if paged {
			l.start = pages[page].start
			if err := printSessionGroups(stdout, pageSessionGroups(groups, pages[page]), format); err != nil {
				return listRow{}, false, err
			}
		} else {
			l.start = 0
			if err := printSessionGroups(stdout, groups, format); err != nil {
				return listRow{}, false, err
			}
		}
		terminal.Print(stdout, footer.String())
		if paged {
			terminal.Println(stdout, pageLine(pages, page, len(rows)))
		}
		if len(rows) == 0 {
			return listRow{}, false, nil
		}
		moved := false
		for !moved {
			terminal.Println(stdout)
			answer, err := p.line(question)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return listRow{}, false, nil
				}
				return listRow{}, false, err
			}
			answer = strings.TrimSpace(answer)
			if answer == "" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "quit") {
				return listRow{}, false, nil
			}
			if paged {
				switch strings.ToLower(answer) {
				case "n", "next":
					if page == len(pages)-1 {
						terminal.Println(stdout, "This is the last page; p goes back.")
						continue
					}
					l.start, moved = pages[page+1].start, true
					continue
				case "p", "prev", "previous":
					if page == 0 {
						terminal.Println(stdout, "This is the first page; n goes on.")
						continue
					}
					l.start, moved = pages[page-1].start, true
					continue
				}
			}
			row, matched := matchBrowseRow(answer, rows)
			if !matched {
				if paged {
					terminal.Println(stdout, "Enter a listed number or unique short SESSION_ID, n or p for another page, or q to quit.")
				} else {
					terminal.Println(stdout, "Enter a listed number or unique short SESSION_ID, or q to quit.")
				}
				continue
			}
			return row, true, nil
		}
		if l.clear != nil {
			l.clear()
		}
	}
}

// pages splits the table's n rows into pages that fit the terminal above
// the footer, the page line, and the prompt. It returns one page holding
// every row when they all fit or the terminal's size is unknown. The pages
// are counted from the top each time, so a resized terminal gets pages of
// the new size.
func (l *sessionPicker) pages(stdout io.Writer, groups []sessionTableGroup, format listFormatOptions, n int, footer, question string) []pickerPage {
	all := []pickerPage{{0, n}}
	width, height, ok := l.env.terminalSize(stdout)
	if !ok || n == 0 {
		return all
	}
	tableLines := func(pg pickerPage) int {
		var buf bytes.Buffer
		_ = printSessionGroups(&buf, pageSessionGroups(groups, pg), format)
		return displayLines(buf.String(), width)
	}
	// Below the table: the footer, a blank line, and the prompt.
	chrome := displayLines(footer, width) + 1 + displayLines(question, width)
	if tableLines(all[0])+chrome <= height {
		return all
	}
	// The widest the page line gets.
	budget := height - chrome - lineRows(pageLine([]pickerPage{{n, n}, {n, n}, {n, n}}, 1, n), width)
	var pages []pickerPage
	for start := 0; start < n; {
		// The most rows that fit: a page's height only grows with its rows.
		lo, hi := 1, n-start
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if tableLines(pickerPage{start, start + mid}) <= budget {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		rows := max(lo, min(minPickerPageRows, n-start))
		pages = append(pages, pickerPage{start, start + rows})
		start += rows
	}
	return pages
}

// pageLine says which rows the page shows and how to reach the others.
func pageLine(pages []pickerPage, page, n int) string {
	line := fmt.Sprintf("Sessions %d-%d of %d ·", pages[page].start+1, pages[page].end, n)
	if page < len(pages)-1 {
		line += " [n] next "
	}
	if page > 0 {
		line += " [p] previous"
	}
	return strings.TrimRight(line, " ")
}

// pageSessionGroups is the part of groups a page shows. A group whose
// earlier rows are on a previous page keeps its heading, marked continued.
func pageSessionGroups(groups []sessionTableGroup, pg pickerPage) []sessionTableGroup {
	var out []sessionTableGroup
	offset := 0
	for _, group := range groups {
		from, to := max(pg.start-offset, 0), min(pg.end-offset, len(group.rows))
		offset += len(group.rows)
		if from >= to {
			continue
		}
		part := group
		part.rows = group.rows[from:to]
		part.continued = from > 0
		out = append(out, part)
	}
	if out == nil {
		// An empty table still prints its column header.
		return groups
	}
	return out
}

// matchBrowseRow resolves a typed answer to one listed row. A unique exact
// short or full SESSION_ID wins first so an all-decimal short ID is not
// mistaken for a row number. A small integer (shorter than a short ID) then
// selects a 1-based index. Otherwise a unique SESSION_ID prefix matches.
func matchBrowseRow(answer string, rows []listRow) (listRow, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return listRow{}, false
	}
	lower := strings.ToLower(answer)
	var exact listRow
	exactMatches := 0
	for _, r := range rows {
		if lower == strings.ToLower(r.SessionID) || lower == strings.ToLower(r.ShortID) {
			exact = r
			exactMatches++
		}
	}
	if exactMatches > 0 {
		return exact, exactMatches == 1
	}
	if n, err := strconv.Atoi(answer); err == nil && len(answer) < minShortSessionID {
		if n >= 1 && n <= len(rows) {
			return rows[n-1], true
		}
		return listRow{}, false
	}
	var match listRow
	matches := 0
	for _, r := range rows {
		id := strings.ToLower(r.SessionID)
		if strings.HasPrefix(id, lower) {
			match = r
			matches++
		}
	}
	if matches == 1 {
		return match, true
	}
	return listRow{}, false
}

// readSessionView reads one session's metadata sidecar and resolves its
// linked sessions' availability: what `show SESSION_ID` prints, without
// the source bundle.
func readSessionView(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (sessionView, error) {
	key, err := locateMetadataKey(ctx, store, harness, sessionID)
	if err != nil {
		return sessionView{}, err
	}
	metadata, err := reader.ReadMetadata(ctx, store, key)
	if err != nil {
		return sessionView{}, err
	}
	return metadataWithLinks(ctx, store, metadata), nil
}

// loadSessionsForBrowse lists metadata with the same filters list uses, for
// interactive show and handoff.
func loadSessionsForBrowse(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string) ([]archive.Metadata, int, bool, error) {
	sessions, err := reader.ListMetadataWithOptions(context.Background(), store, archiveSessionsPrefix, opts.filter, reader.ListOptions{Cache: listCache(env, opts.noCache), Skipped: warnSkippedSidecar(stderr, command)})
	if err != nil {
		return nil, 0, false, err
	}
	sessions = filterListOrigin(sessions, opts.imported, opts.hookCaptured)
	shown, totalMatched, truncated := applyListLimit(sessions, opts.limit)
	return shown, totalMatched, truncated, nil
}

// browseSessions is the newest sessions bare show and handoff offer, with
// the format to list them in.
type browseSessions struct {
	sessions     []archive.Metadata
	totalMatched int
	truncated    bool
	format       listFormatOptions
}

// findBrowseSessions loads browseSessions. ok is false, with code 0, when
// none match (after saying so), and with code 1 after an error.
func findBrowseSessions(env sessionSelectionDependencies, store storage.ObjectStore, cfg config.Config, stdout, stderr io.Writer, harness, command string) (found browseSessions, ok bool, code int) {
	stopBrowse := startActivity(stdout, "Finding sessions…")
	shown, totalMatched, truncated, err := loadSessionsForBrowse(env, store, listOptions{filter: reader.Filter{Harness: harness}, limit: defaultListLimit}, stderr, command)
	stopBrowse()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
		return browseSessions{}, false, 1
	}
	if totalMatched == 0 {
		terminal.Println(stdout, "No archived sessions match.")
		return browseSessions{}, false, 0
	}
	format := listFormatOptions{Now: env.now(), Projects: projectLabels(cfg), Style: styleFor(stdout), GroupByProject: true}
	return browseSessions{sessions: shown, totalMatched: totalMatched, truncated: truncated, format: format}, true, 0
}

// selectArchivedSession is handoff's (and `show --json`'s) one-shot picker.
// It returns selected=false when the archive is empty or the user quits.
func selectArchivedSession(env sessionSelectionDependencies, store storage.ObjectStore, cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, harness, command, action string) (row listRow, selected bool, code int) {
	found, ok, code := findBrowseSessions(env, store, cfg, stdout, stderr, harness, command)
	if !ok {
		return listRow{}, false, code
	}
	row, selected, err := pickBrowseSession(env, newPrompter(stdin, stdout), stdout, found.sessions, found.totalMatched, found.truncated, found.format, action)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
		return listRow{}, false, 1
	}
	return row, selected, 0
}

// saveTerminalState records the terminal modes of in, when it is a
// terminal, and returns a function that restores them.
func saveTerminalState(in io.Reader) (restore func()) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return func() {}
	}
	state, err := term.GetState(int(file.Fd()))
	if err != nil {
		return func() {}
	}
	return func() { _ = term.Restore(int(file.Fd()), state) }
}
