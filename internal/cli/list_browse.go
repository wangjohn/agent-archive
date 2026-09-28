package cli

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// browseInteractive reports whether list/show should offer a session picker:
// both stdin and stdout are terminals, and the caller is not forcing JSON.
// Interactive list skips the pager so the prompt stays with the table.
func browseInteractive(env Env, stdin io.Reader, stdout io.Writer, jsonOut bool) bool {
	return !jsonOut && env.isTerminal(stdin) && env.isTerminal(stdout)
}

// runSessionBrowser prints a numbered session table and lets the user pick
// sessions to show. When loop is true (list), it keeps prompting until q;
// when false (bare show), it shows one selection and returns.
func runSessionBrowser(stdin io.Reader, stdout, stderr io.Writer, store storage.ObjectStore, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions, loop bool) int {
	for {
		row, ok, code := pickBrowseSession(stdin, stdout, stderr, sessions, totalMatched, truncated, format)
		if code != 0 {
			return code
		}
		if !ok {
			return 0
		}
		if code := showSessionMetadata(stdout, stderr, store, row.SessionID, row.HarnessKey); code != 0 {
			return code
		}
		if !loop {
			return 0
		}
	}
}

// pickBrowseSession prints the numbered table once and prompts until the user
// selects a row (ok=true), quits (ok=false, code=0), or an error occurs.
func pickBrowseSession(stdin io.Reader, stdout, stderr io.Writer, sessions []archive.Metadata, totalMatched int, truncated bool, format listFormatOptions) (row listRow, ok bool, code int) {
	format.Numbered = true
	rows := formatSessionRows(sessions, format)
	if err := printSessionTable(stdout, rows, format); err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return listRow{}, false, 1
	}
	printListFooter(stdout, len(sessions), totalMatched, truncated)
	if len(rows) == 0 {
		return listRow{}, false, 0
	}
	p := newPrompter(stdin, stdout)
	for {
		terminal.Println(stdout)
		answer, err := p.line(p.promptText("Enter number (or short SESSION_ID) to show, or q to quit", true, nil, -1, ": "))
		if err != nil {
			if strings.Contains(err.Error(), "no more input") {
				return listRow{}, false, 0
			}
			terminal.Printf(stderr, "agent-archive: %v\n", err)
			return listRow{}, false, 1
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "quit") {
			return listRow{}, false, 0
		}
		row, matched := matchBrowseRow(answer, rows)
		if !matched {
			terminal.Println(stdout, "Enter a listed number or short SESSION_ID, or q to quit.")
			continue
		}
		return row, true, 0
	}
}

// matchBrowseRow resolves a typed answer to one listed row. Exact short or
// full SESSION_ID matches win first so an all-decimal short id is never
// mistaken for a row number. A small integer (shorter than a short id) then
// selects a 1-based index. Otherwise a unique SESSION_ID prefix matches.
func matchBrowseRow(answer string, rows []listRow) (listRow, bool) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return listRow{}, false
	}
	lower := strings.ToLower(answer)
	for _, r := range rows {
		if lower == strings.ToLower(r.SessionID) || lower == strings.ToLower(r.ShortID) {
			return r, true
		}
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

// showSessionMetadata prints one session's metadata sidecar (with live link
// availability), the same default path as `show SESSION_ID` without
// --normalized.
func showSessionMetadata(stdout, stderr io.Writer, store storage.ObjectStore, sessionID, harness string) int {
	ctx := context.Background()
	key, err := locateMetadataKey(ctx, store, harness, sessionID)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	metadata, err := reader.ReadMetadata(ctx, store, key)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	return printJSON(stdout, stderr, metadataWithLinks(ctx, store, metadata))
}

// loadSessionsForBrowse lists metadata with the same filters list uses, for
// bare interactive show.
func loadSessionsForBrowse(env Env, store storage.ObjectStore, opts listOptions, stderr io.Writer) ([]archive.Metadata, int, bool, error) {
	sessions, err := reader.ListMetadataWithOptions(context.Background(), store, archiveSessionsPrefix, opts.filter, reader.ListOptions{Cache: listCache(env, opts.noCache), Skipped: warnSkippedSidecar(stderr, "show")})
	if err != nil {
		return nil, 0, false, err
	}
	sessions = filterListOrigin(sessions, opts.imported, opts.hookCaptured)
	shown, totalMatched, truncated := applyListLimit(sessions, opts.limit)
	return shown, totalMatched, truncated, nil
}
