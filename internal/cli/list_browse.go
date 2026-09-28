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
	format.Numbered = true
	rows := formatSessionRows(sessions, format)
	if err := printSessionTable(stdout, rows, format); err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	printListFooter(stdout, len(sessions), totalMatched, truncated)
	if len(rows) == 0 {
		return 0
	}
	p := newPrompter(stdin, stdout)
	for {
		terminal.Println(stdout)
		answer, err := p.line(p.promptText("Enter number (or short SESSION_ID) to show, or q to quit", true, nil, -1, ": "))
		if err != nil {
			if strings.Contains(err.Error(), "no more input") {
				return 0
			}
			terminal.Printf(stderr, "agent-archive: %v\n", err)
			return 1
		}
		answer = strings.TrimSpace(answer)
		if answer == "" || strings.EqualFold(answer, "q") || strings.EqualFold(answer, "quit") {
			return 0
		}
		row, ok := matchBrowseRow(answer, rows)
		if !ok {
			terminal.Println(stdout, "Enter a listed number or short SESSION_ID, or q to quit.")
			continue
		}
		if code := showSessionMetadata(stdout, stderr, store, row.SessionID, ""); code != 0 {
			return code
		}
		if !loop {
			return 0
		}
	}
}

// matchBrowseRow resolves a typed answer to one listed row: a 1-based index,
// a short id unique in the table, or a unique prefix of a session id.
func matchBrowseRow(answer string, rows []listRow) (listRow, bool) {
	if n, err := strconv.Atoi(answer); err == nil {
		if n >= 1 && n <= len(rows) {
			return rows[n-1], true
		}
		return listRow{}, false
	}
	answer = strings.ToLower(answer)
	var match listRow
	matches := 0
	for _, r := range rows {
		id := strings.ToLower(r.SessionID)
		short := strings.ToLower(r.ShortID)
		if answer == id || answer == short {
			return r, true
		}
		if strings.HasPrefix(id, answer) {
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
