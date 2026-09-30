package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// handoffTitleScanLimit is how many of this Mac's sessions, newest activity
// first, a title search reads. A local session's title is its first prompt,
// which takes reading its transcript, so the search is bounded as the picker
// is; a session past it that was uploaded is found in the archive instead.
const handoffTitleScanLimit = defaultListLimit

// archiveSessionIDLength is the length of an archive session ID, 32 lower-case
// hexadecimal digits: the one shape worth a direct archive read before
// searching titles (as show does), so a title as long is never sent there.
const archiveSessionIDLength = 32

// handoffQueryWidth is how many columns of the query a message repeats, so a
// pasted paragraph is not echoed back in full.
const handoffQueryWidth = 80

func isArchiveSessionID(query string) bool {
	if len(query) != archiveSessionIDLength {
		return false
	}
	for _, c := range query {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// queryLabel is the query as a message names it: one printable line, cut to
// handoffQueryWidth.
func queryLabel(query string) string {
	return ellipsize(archive.DisplayLine(query), handoffQueryWidth)
}

// skippedSessions is the calling agent's own session, which a title never
// offers: the person names another. A direct handoff (--to) starts from it.
func skippedSessions(opts *handoffOptions, env currentSessionDependencies) map[string]bool {
	if opts.to != "" {
		return nil
	}
	return currentSessions(env)
}

// handoffCandidateLimit is how many matching sessions a title lists, on stderr
// or in the picker, before saying how many more there are. The caller may be
// an agent, whose context a title as common as "fix" must not flood.
const handoffCandidateLimit = 20

// resolveHandoffQuery turns the positional argument, a session ID or a title,
// into the one session to hand off, replacing opts.sessionID (and opts.harness
// when it was not given) with that session's archive ID and harness. It
// matches as show does (a title substring, a short ID or an ID prefix, an
// exact ID winning) over titles and metadata, never transcript content:
//
//  1. a session ID registered on this Mac (a subagent's too, as handoff
//     always took), then, for a full ID, the archive (one read), which a
//     title match may not shadow;
//  2. this Mac's sessions, which need no network;
//  3. only when none of those match, the archive's.
//
// Several matches print the candidates to stderr and exit 1 without a
// terminal, and open the handoff picker limited to them with one. done is set
// when the command should exit with code instead of handing off.
func resolveHandoffQuery(opts *handoffOptions, home string, interactive bool, stdin io.Reader, stdout, stderr io.Writer, env handoffCommandDependencies) (code int, done bool) {
	// Titles are stored as one line of single spaces, so a query with a
	// newline or a doubled space is matched as that line.
	query := strings.Join(strings.Fields(opts.sessionID), " ")
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		// resolveHandoffTarget reports both.
		return 0, false
	}
	r := handoffQueryResolver{opts: opts, query: query, skip: skippedSessions(opts, env), home: home, cfg: cfg, interactive: interactive, stdin: stdin, stdout: stdout, stderr: stderr, env: env}
	code, done = r.resolve()
	if done {
		return code, true
	}
	// The ID names a registration file and bucket keys and, however it was
	// found, may have come from stored data: the parse no longer vets it.
	harness := opts.harness
	if harness == "" {
		harness = archive.HarnessClaude
	}
	if _, err := archive.MetadataObjectKey(harness, opts.sessionID); err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: the matching session has an unusable ID: %v\n", err)
		return 1, true
	}
	return 0, false
}

type handoffQueryResolver struct {
	opts        *handoffOptions
	query       string
	home        string
	cfg         config.Config
	interactive bool
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	env         handoffCommandDependencies

	// skip holds the native IDs of the agent session running the command,
	// which a title does not offer (as --latest passes over it).
	skip  map[string]bool
	store storage.ObjectStore
	// archiveErr is why the archive could not be read, for the no-match
	// message.
	archiveErr error
}

func (r *handoffQueryResolver) resolve() (code int, done bool) {
	ctx := context.Background()
	opts := r.opts
	var regs []archive.SessionRegistration
	if opts.source != "archive" {
		var err error
		if regs, err = state.OpenReadOnly(r.home).LoadRegistrations(); err != nil {
			terminal.Printf(r.stderr, "agent-archive: handoff: note: could not read this Mac's sessions, searching the archive only: %v\n", err)
		}
		for _, reg := range regs {
			if reg.ArchiveSessionID == r.query && (opts.harness == "" || archive.CanonicalHarness(reg.Harness.Name) == opts.harness) {
				opts.sessionID = r.query
				return 0, false
			}
		}
	}
	if opts.source != "local" && isArchiveSessionID(r.query) {
		if harness, ok := r.exactArchiveSession(ctx); ok {
			opts.sessionID, opts.harness = r.query, harness
			return 0, false
		}
	}
	scanned := false
	if opts.source != "archive" {
		stop := startActivity(r.stdout, "Finding sessions…")
		picker := handoffPicker{ctx: ctx, env: r.env, home: r.home, harness: opts.harness, source: "local"}
		rows, _, truncated := picker.rows(regs, nil, handoffTitleScanLimit)
		stop()
		scanned = truncated
		if matches := matchHandoffRows(rows, r.query, r.skip); len(matches) > 0 {
			return r.choose(matches)
		}
	}
	if opts.source != "local" {
		matches, err := r.archiveMatches()
		if err != nil {
			r.archiveErr = err
		} else if len(matches) > 0 {
			return r.choose(matches)
		}
	}
	terminal.Println(r.stderr, r.noMatchMessage(scanned))
	return 1, true
}

// openArchive opens the archive's store once.
func (r *handoffQueryResolver) openArchive() (storage.ObjectStore, error) {
	if r.store != nil {
		return r.store, nil
	}
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	r.store = store
	return store, nil
}

// exactArchiveSession reads the archive for the query as a full session ID.
// A miss, and an archive that cannot be read, both leave the title search to
// go on.
func (r *handoffQueryResolver) exactArchiveSession(ctx context.Context) (harness string, ok bool) {
	store, err := r.openArchive()
	if err != nil {
		r.archiveErr = err
		return "", false
	}
	harnesses := reader.Harnesses
	if r.opts.harness != "" {
		harnesses = []string{r.opts.harness}
	}
	var found []string
	for _, name := range harnesses {
		key, err := archive.MetadataObjectKey(name, r.query)
		if err != nil {
			return "", false
		}
		if _, err := store.Get(ctx, key); err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				r.archiveErr = err
				return "", false
			}
			continue
		}
		found = append(found, name)
	}
	// The same ID under several harnesses is not guessed; the title search
	// reports whatever else matches.
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// archiveMatches lists the archive's top-level sessions and matches the query
// against their titles and IDs.
func (r *handoffQueryResolver) archiveMatches() ([]handoffPickerRow, error) {
	store, err := r.openArchive()
	if err != nil {
		return nil, err
	}
	stop := startActivity(r.stdout, "Finding sessions…")
	sessions, _, _, err := loadSessionsForBrowse(r.env, store, listOptions{filter: reader.Filter{Harness: r.opts.harness}}, r.stderr, "handoff")
	stop()
	if err != nil {
		return nil, err
	}
	rows := make([]handoffPickerRow, 0, len(sessions))
	for _, m := range topLevelSessions(sessions) {
		rows = append(rows, handoffPickerRow{metadata: m, active: m.CapturedAt})
	}
	return matchHandoffRows(rows, r.query, r.skip), nil
}

// matchHandoffRows keeps the rows matchSessionsByQuery matches, in order,
// leaving out the sessions whose native ID is in skip.
func matchHandoffRows(rows []handoffPickerRow, query string, skip map[string]bool) []handoffPickerRow {
	rows = slices.DeleteFunc(slices.Clone(rows), func(row handoffPickerRow) bool { return skip[row.metadata.NativeSessionID] })
	sessions := make([]archive.Metadata, len(rows))
	for i, row := range rows {
		sessions[i] = row.metadata
	}
	// One ID can be published under two harnesses, so the harness is part
	// of the key.
	byKey := make(map[string]handoffPickerRow, len(rows))
	for _, row := range rows {
		byKey[handoffRowKey(row.metadata)] = row
	}
	var matched []handoffPickerRow
	for _, m := range matchSessionsByQuery(sessions, query) {
		matched = append(matched, byKey[handoffRowKey(m)])
	}
	return matched
}

func handoffRowKey(m archive.Metadata) string {
	return m.Harness.Name + "/" + m.SessionID
}

// choose settles on one of several or one matching row.
func (r *handoffQueryResolver) choose(matches []handoffPickerRow) (code int, done bool) {
	row := matches[0]
	if len(matches) > 1 {
		format := listFormatOptions{Now: r.env.now(), Projects: projectLabels(r.cfg)}
		if !r.interactive {
			r.printCandidates(matches, format)
			return 1, true
		}
		format.Style, format.GroupByProject, format.Numbered = styleFor(r.stdout), true, true
		format.NarrowHint = "Narrow with more of the title, or --harness, or name a session: agent-archive handoff SESSION_ID."
		shown := matches[:min(len(matches), handoffCandidateLimit)]
		picked, selected, err := pickBrowseRow(newPrompter(r.stdin, r.stdout), r.stdout, formatHandoffRows(shown, format), len(matches), len(shown) < len(matches), format, "hand off")
		if err != nil {
			terminal.Printf(r.stderr, "agent-archive: handoff: %v\n", err)
			return 1, true
		}
		if !selected {
			return 0, true
		}
		i := slices.IndexFunc(shown, func(m handoffPickerRow) bool {
			return m.metadata.SessionID == picked.SessionID && m.metadata.Harness.Name == picked.HarnessKey
		})
		if i < 0 {
			// Never a guess: the picked row is one of those listed.
			terminal.Println(r.stderr, "agent-archive: handoff: the picked session is not one of those listed")
			return 1, true
		}
		row = shown[i]
	}
	r.opts.sessionID, r.opts.harness = row.metadata.SessionID, row.metadata.Harness.Name
	return 0, false
}

// printCandidates lists ambiguous matches for a caller with no terminal to
// pick on: a person reading a log, or an agent that must ask which.
func (r *handoffQueryResolver) printCandidates(matches []handoffPickerRow, format listFormatOptions) {
	var b strings.Builder
	fmt.Fprintf(&b, "agent-archive: handoff: %q matches %d sessions; pass one SESSION_ID, or run on a terminal to pick:\n", queryLabel(r.query), len(matches))
	shown := matches[:min(len(matches), handoffCandidateLimit)]
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, row := range formatHandoffRows(shown, format) {
		// The table is built in memory, where writes cannot fail. The ID is
		// stored data, like the title, and may hold control characters.
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", archive.DisplayLine(row.ShortID), row.Harness, row.Project, row.When, row.Title)
	}
	_ = tw.Flush()
	if len(shown) < len(matches) {
		fmt.Fprintf(&b, "  ... and %d more; use more of the title, or --harness, to narrow\n", len(matches)-len(shown))
	}
	terminal.Print(r.stderr, b.String())
}

func (r *handoffQueryResolver) noMatchMessage(scanLimited bool) string {
	where, ok := map[string]string{"local": "on this Mac", "archive": "in the archive"}[r.opts.source]
	if !ok {
		where = "on this Mac or in the archive"
	}
	message := fmt.Sprintf("agent-archive: handoff: no session matches %q %s (see `agent-archive list`)", queryLabel(r.query), where)
	if r.opts.harness != "" {
		message += " for " + r.opts.harness
	}
	if scanLimited {
		message += fmt.Sprintf("; only this Mac's %d most recently active sessions were searched", handoffTitleScanLimit)
	}
	if r.archiveErr != nil {
		message += fmt.Sprintf("\nagent-archive: handoff: note: the archive could not be read, so only this Mac's sessions were searched: %v", r.archiveErr)
	}
	return message
}
