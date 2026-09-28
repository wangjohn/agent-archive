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
// lookups win. Otherwise it matches short ids and case-insensitive title
// substrings among archived metadata (capped like list). Multiple matches on
// a TTY open a one-shot picker that returns the chosen session (so the caller
// can still honor --normalized); off a TTY they error with the candidates.
func resolveShowQuery(ctx context.Context, store storage.ObjectStore, env Env, stdin io.Reader, stdout, stderr io.Writer, harness, query string, cfgProjects map[string]string) (showLookup, int) {
	key, err := locateMetadataKey(ctx, store, harness, query)
	if err == nil {
		parts := strings.Split(strings.TrimPrefix(key, archiveSessionsPrefix+"/"), "/")
		if len(parts) >= 2 {
			return showLookup{SessionID: parts[1], Harness: parts[0]}, 0
		}
		return showLookup{SessionID: query, Harness: harness}, 0
	}
	// Exact-id miss or a query that is not a valid SESSION_ID (spaces,
	// punctuation) may fall through to short-id / title search. Ambiguous
	// harnesses, explicit --harness, and storage errors stop here.
	miss := strings.Contains(err.Error(), "no archived session") ||
		strings.Contains(err.Error(), "invalid archive session ID")
	if harness != "" || !miss {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}

	sessions, err := reader.ListMetadataWithOptions(ctx, store, archiveSessionsPrefix, reader.Filter{}, reader.ListOptions{
		Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "show"),
	})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}
	shown, _, _ := applyListLimit(sessions, defaultListLimit)
	matches := matchSessionsByQuery(shown, query)
	switch len(matches) {
	case 0:
		terminal.Printf(stderr, "agent-archive: show: no archived session %q (see `agent-archive list`)\n", archive.DisplayLine(query))
		return showLookup{}, 1
	case 1:
		return showLookup{SessionID: matches[0].SessionID, Harness: matches[0].Harness.Name}, 0
	}
	if !browseInteractive(env, stdin, stdout, false) {
		terminal.Printf(stderr, "agent-archive: show: %q matches %d sessions; pass a SESSION_ID or run show on a terminal to pick one\n", archive.DisplayLine(query), len(matches))
		for _, m := range matches {
			label := m.Title
			if label == "" {
				label = m.SessionID
			}
			terminal.Printf(stderr, "  %s  %s  %s\n", archive.DisplayLine(m.Harness.Name), archive.DisplayLine(shortSessionID(m.SessionID)), archive.DisplayLine(label))
		}
		return showLookup{}, 1
	}
	format := listFormatOptions{Now: env.now(), Projects: cfgProjects, Style: styleFor(stdout), GroupByProject: true}
	row, ok, code := pickBrowseSession(stdin, stdout, stderr, matches, len(matches), false, format)
	if code != 0 {
		return showLookup{}, code
	}
	if !ok {
		return showLookup{Cancelled: true}, 0
	}
	return showLookup{SessionID: row.SessionID, Harness: row.HarnessKey}, 0
}

// matchSessionsByQuery returns sessions whose short id, full id, or title
// contains query (case-insensitive). Exact short/full id matches alone win
// when any are present.
func matchSessionsByQuery(sessions []archive.Metadata, query string) []archive.Metadata {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	lower := strings.ToLower(query)
	var exact, fuzzy []archive.Metadata
	shorts := uniqueShortIDs(sessionIDs(sessions))
	for i, m := range sessions {
		idLower := strings.ToLower(m.SessionID)
		shortLower := strings.ToLower(shorts[i])
		if idLower == lower || shortLower == lower {
			exact = append(exact, m)
			continue
		}
		if strings.HasPrefix(idLower, lower) || strings.Contains(strings.ToLower(m.Title), lower) {
			fuzzy = append(fuzzy, m)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fuzzy
}

func sessionIDs(sessions []archive.Metadata) []string {
	ids := make([]string, len(sessions))
	for i, m := range sessions {
		ids[i] = m.SessionID
	}
	return ids
}

func shortSessionID(id string) string {
	if len(id) > minShortSessionID {
		return id[:minShortSessionID]
	}
	return id
}
