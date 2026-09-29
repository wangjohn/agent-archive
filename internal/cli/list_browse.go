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
// screen (altScreen). Transcript content is shown only when t is pressed.
type sessionBrowser struct {
	env     sessionBrowserDependencies
	prompt  *prompter
	stdout  io.Writer
	stderr  io.Writer
	store   storage.ObjectStore
	format  listFormatOptions
	noPager bool
	screen  *altScreen
	// last is the session whose details were shown last, printed to the
	// normal screen on the way out so its ID stays in scrollback.
	last *sessionView
}

// runSessionBrowser browses sessions until the user quits (q, an empty
// answer at the list, or end of input), then prints the last session viewed.
func runSessionBrowser(env sessionBrowserDependencies, p *prompter, stdout, stderr io.Writer, store storage.ObjectStore, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, noPager bool, command string) int {
	b := &sessionBrowser{env: env, prompt: p, stdout: stdout, stderr: stderr, store: store, format: format, noPager: noPager, screen: enterAltScreen(stdout, env)}
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
		row, ok, err := pickBrowseSession(b.prompt, b.stdout, sessions, totalMatched, truncated, b.format, "show")
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
		renderSessionSummary(b.stdout, view, b.summaryOptions(false))
		action, err := b.detailsPrompt(view, row)
		if err != nil || action != browseRedraw {
			return action, err
		}
	}
}

func (b *sessionBrowser) detailsPrompt(view sessionView, row listRow) (browseAction, error) {
	for {
		terminal.Println(b.stdout)
		answer, err := b.prompt.line(b.prompt.promptText("[t] transcript  [Enter/b] back to list  [q] quit", false, nil, -1, ": "))
		if err != nil {
			return endOfInput(err)
		}
		switch strings.ToLower(answer) {
		case "", "b", "back":
			return browseBack, nil
		case "q", "quit":
			return browseQuit, nil
		case "t", "transcript":
			action, err := b.transcript(view, row)
			if err != nil || action != browseStay {
				return action, err
			}
		default:
			terminal.Println(b.stdout, "Enter t for the transcript, b (or just Enter) for the list, or q to quit.")
		}
	}
}

// transcript downloads and verifies the session's bundle and shows its
// transcript through the pager. A bundle that cannot be read is reported
// under the details, which stay open.
func (b *sessionBrowser) transcript(view sessionView, row listRow) (browseAction, error) {
	ctx := context.Background()
	stop := startActivity(b.stdout, "Loading transcript…")
	text, err := b.renderTranscript(ctx, row)
	stop()
	if err != nil {
		terminal.Printf(b.stderr, "agent-archive: show: %s\n", describeBundleError(err, row.SessionID, "--transcript"))
		return browseStay, nil
	}
	b.screen.clear()
	restoreTerminal := saveTerminalState(b.prompt.source)
	pagerCtx, stopPager := context.WithCancel(ctx)
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
	// once: wait, so the transcript stays on screen until the user asks
	// for the details again.
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

// pickBrowseSession prints the numbered table once and prompts until the user
// selects a row (ok=true), quits or input ends (ok=false), or an error occurs.
func pickBrowseSession(p *prompter, stdout io.Writer, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, action string) (row listRow, ok bool, err error) {
	format.Numbered = true
	rows := formatSessionRows(sessions, format)
	if err := printSessionTable(stdout, rows, format); err != nil {
		return listRow{}, false, err
	}
	printListFooter(stdout, len(sessions), totalMatched, truncated)
	if len(rows) == 0 {
		return listRow{}, false, nil
	}
	for {
		terminal.Println(stdout)
		answer, err := p.line(p.promptText("Enter number (or unique short SESSION_ID) to "+action+", or q to quit", true, nil, -1, ": "))
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
		row, matched := matchBrowseRow(answer, rows)
		if !matched {
			terminal.Println(stdout, "Enter a listed number or unique short SESSION_ID, or q to quit.")
			continue
		}
		return row, true, nil
	}
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
	row, selected, err := pickBrowseSession(newPrompter(stdin, stdout), stdout, found.sessions, found.totalMatched, found.truncated, found.format, action)
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
