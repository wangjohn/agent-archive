package cli

import (
	"context"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// showLookup is the result of resolving a show argument to one session.
type showLookup struct {
	SessionID string
	Harness   string
	// Cancelled is set when the user quit an interactive disambiguation
	// picker without choosing a session (caller should exit 0).
	Cancelled bool
}

// resolveShowQuery turns a show argument into a session. Exact SESSION_ID
// lookups win. Otherwise the argument is words, matched as list matches them
// (sessionQuery) over every archived sidecar, in the tiers of the search: the
// working directory's repository first, then every project, subagents last.
// Several matches on a terminal open the browser over them, with the words in
// its filter: it shows the sessions' details itself (and the lookup is
// Cancelled, nothing left for the caller to read), unless pickOne is set
// because the caller has something to do with one session (--transcript or
// --json), when it returns the chosen session. Off a terminal they print the
// candidates and exit 1.
func resolveShowQuery(ctx context.Context, store storage.ObjectStore, env showQueryDependencies, stdin io.Reader, stdout, stderr io.Writer, harness, query string, cfgProjects map[string]string, noPager, pickOne bool) (showLookup, int) {
	// Full archive IDs use the direct-read path. With --harness, a short ID
	// or title would otherwise be mistaken for a literal object key.
	if harness == "" || len(query) == 32 {
		key, err := locateMetadataKey(ctx, store, harness, query)
		if err == nil {
			parts := strings.Split(strings.TrimPrefix(key, archiveSessionsPrefix+"/"), "/")
			if len(parts) >= 2 {
				return showLookup{SessionID: parts[1], Harness: parts[0]}, 0
			}
			return showLookup{SessionID: query, Harness: harness}, 0
		}
		// Exact-id misses fall through to short-id / title search. Keep
		// ambiguous harnesses and storage failures as errors.
		miss := strings.Contains(err.Error(), "no archived session") ||
			strings.Contains(err.Error(), "invalid archive session ID")
		if !miss {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return showLookup{}, 1
		}
	}

	scope, err := scopeFor(env, "", false)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}
	stopSearch := startActivity(stdout, "Finding sessions…")
	sessions, err := reader.ListMetadataWithOptions(ctx, store, archiveSessionsPrefix, reader.Filter{Harness: harness}, reader.ListOptions{
		Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "show"),
	})
	stopSearch()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}
	// Search every listed sidecar — do not apply list's --limit window, or
	// older title matches would be silently invisible.
	found := searchSessions(sessions, parseSessionQuery(query), scope, func(m archive.Metadata) sessionFields {
		return fieldsOf(m, sessionProjectName(m, cfgProjects))
	})
	matches := found.matches
	if note := found.outsideNote(scope); note != "" {
		terminal.Println(stderr, note)
	}
	switch len(matches) {
	case 0:
		terminal.Printf(stderr, "agent-archive: show: no archived session %q (see `agent-archive list`)\n", archive.DisplayLine(query))
		return showLookup{}, 1
	case 1:
		return showLookup{SessionID: matches[0].SessionID, Harness: matches[0].Harness.Name}, 0
	}
	format := listFormatOptions{Now: env.now(), Projects: cfgProjects, Children: childCounts(sessions)}
	if !browseInteractive(env, stdin, stdout) {
		shown := matches[:min(len(matches), handoffCandidateLimit)]
		label := ""
		if found.inScope {
			label = scope.Label
		}
		var listFlags []string
		if harness != "" {
			listFlags = append(listFlags, "--harness "+harness)
		}
		candidateList{
			command: "show", query: query, label: label, total: len(matches), rows: formatSessionRows(shown, format), listFlags: listFlags,
			next: func(row listRow) string { return "agent-archive show " + archive.DisplayLine(row.ShortID) },
		}.print(stderr)
		return showLookup{}, 1
	}
	format.Style, format.GroupByProject, format.Numbered = styleFor(stdout), true, true
	spec := browserSpec{Mode: browseSessions, Choices: rowChoices(formatSessionRows(matches, format), format), Query: strings.Join(strings.Fields(query), " "), Command: "show", Store: store, NoPager: noPager}
	if pickOne {
		spec.Mode, spec.Verb = pickSession, "Show"
	}
	row, picked, code := runBrowser(env, newPrompter(stdin, stdout), stdout, stderr, spec)
	if code != 0 {
		return showLookup{}, code
	}
	if !picked {
		return showLookup{Cancelled: true}, 0
	}
	return showLookup{SessionID: row.SessionID, Harness: row.HarnessKey}, 0
}

// shortSessionID is the first minShortSessionID bytes of id, cut on a
// character boundary.
func shortSessionID(id string) string {
	return archive.TruncateUTF8(id, minShortSessionID)
}
