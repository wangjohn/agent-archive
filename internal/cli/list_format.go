package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// minShortSessionID is the default visible prefix of a session ID in the
// human list. Longer prefixes are used only when two rows would otherwise
// share the same short ID.
const minShortSessionID = 8

// listRow is one session as the human list table renders it.
type listRow struct {
	Index      int
	SessionID  string
	ShortID    string
	HarnessKey string // raw harness name for show / locateMetadataKey
	ProjectID  string // raw identity; display names can collide
	Title      string // display title (metadata title, else short ID)
	When       string
	CapturedAt string
	Harness    string
	Project    string
	Model      string
	ModelAll   string
	SkillHint  string
	Skills     string
	Origin     string
	Parser     string
	// PR is the pull request the session created last, as "#213"; empty when
	// it created none.
	PR string
	// Live is set for a session this machine saw active within
	// activeSourceWindow, which the picker marks with a dot.
	Live bool
}

// listFormatOptions controls how session rows are built and printed.
type listFormatOptions struct {
	Now            time.Time
	Verbose        bool
	Numbered       bool
	GroupByProject bool
	Projects       map[string]string // project_id → display label (basename)
	Style          textStyle
	NarrowHint     string // the truncation footer's advice; "" for list's flags

	// The rest lay out the human table for the rows it is given (see
	// withColumns). Left unset, every column is shown.
	ShowPR      bool // a PR column, for rows where some session has one
	HideHarness bool // every row has the same harness, which the heading names
	HideProject bool // every row has the same project, which the heading names
	LiveMarks   bool // some row is live: each title leaves room for the dot
	DimID       bool // the ID is for copying, not for choosing: draw it dim
}

// formatSessionRows builds display rows for sessions. Short IDs are unique
// within the returned set (lengthened past 8 characters when needed).
func formatSessionRows(sessions []archive.Metadata, opts listFormatOptions) []listRow {
	ids := make([]string, len(sessions))
	for i, m := range sessions {
		ids[i] = m.SessionID
	}
	shorts := uniqueShortIDs(ids)
	rows := make([]listRow, len(sessions))
	for i, m := range sessions {
		models := modelNames(m)
		skills := skillNames(m)
		project := "-"
		if m.ProjectName != "" {
			project = m.ProjectName
		} else if label := opts.Projects[m.ProjectID]; label != "" {
			project = label
		}
		model := "-"
		modelAll := listOrDash(models)
		if len(models) > 0 {
			model = archive.DisplayLine(models[0])
			if len(models) > 1 {
				model += fmt.Sprintf(" +%d", len(models)-1)
			}
		}
		skillHint := ""
		if len(skills) > 0 {
			skillHint = " · " + archive.DisplayLine(skills[0])
			if len(skills) > 1 {
				skillHint += fmt.Sprintf(" +%d", len(skills)-1)
			}
		}
		title := shorts[i]
		if strings.TrimSpace(m.Title) != "" {
			title = archive.DisplayLine(m.Title)
		}
		rows[i] = listRow{
			PR:         createdPRLabel(m),
			Index:      i + 1,
			SessionID:  m.SessionID,
			ShortID:    shorts[i],
			HarnessKey: m.Harness.Name,
			ProjectID:  m.ProjectID,
			Title:      title,
			When:       relativeAge(opts.Now, m.CapturedAt),
			CapturedAt: formatTimeOrNever(m.CapturedAt),
			Harness:    archive.DisplayLine(m.Harness.Name),
			Project:    archive.DisplayLine(project),
			Model:      model,
			ModelAll:   modelAll,
			SkillHint:  skillHint,
			Skills:     listOrDash(skills),
			Origin:     sessionOrigin(m),
			Parser:     archive.DisplayLine(string(m.Parser.Status)),
		}
	}
	return rows
}

// uniqueShortIDs returns a short prefix for each id, starting at
// minShortSessionID and growing until every prefix is unique within ids.
func uniqueShortIDs(ids []string) []string {
	out := make([]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	maxLen := 0
	for _, id := range ids {
		if len(id) > maxLen {
			maxLen = len(id)
		}
	}
	for length := minShortSessionID; ; length++ {
		seen := map[string]int{}
		for i, id := range ids {
			s := id
			if len(s) > length {
				s = s[:length]
			}
			out[i] = s
			seen[s]++
		}
		ambiguous := false
		for _, n := range seen {
			if n > 1 {
				ambiguous = true
				break
			}
		}
		if !ambiguous || length >= maxLen {
			return out
		}
	}
}

// projectLabels maps configured project IDs to a basename label for list.
// Excluded projects and empty roots are omitted.
func projectLabels(cfg config.Config) map[string]string {
	labels := make(map[string]string)
	for _, p := range cfg.Archive.Projects {
		if !p.Included || p.ProjectID == "" || strings.TrimSpace(p.Root) == "" {
			continue
		}
		base := filepath.Base(filepath.Clean(p.Root))
		if base == "" {
			continue
		}
		if base == "." {
			continue
		}
		if base == string(filepath.Separator) {
			continue
		}
		labels[p.ProjectID] = base
	}
	return labels
}

// printListFooter writes the trailing count / truncation line. narrowHint
// replaces the advice for a command without --limit and --since.
func printListFooter(w io.Writer, shown, totalMatched int, truncated bool, narrowHint string) {
	if truncated {
		switch {
		case totalMatched < 0 && narrowHint != "":
			terminal.Printf(w, "Showing %d or more session(s). %s\n", shown, narrowHint)
		case totalMatched < 0:
			terminal.Printf(w, "Showing %d or more session(s). Use --limit 0 for an exact count, or narrow with --since / --harness.\n", shown)
		case narrowHint != "":
			terminal.Printf(w, "Showing %d of %d session(s). %s\n", shown, totalMatched, narrowHint)
		default:
			terminal.Printf(w, "Showing %d of %d session(s). Use --limit 0 for all, or narrow with --since / --harness.\n", shown, totalMatched)
		}
		return
	}
	terminal.Printf(w, "%d session(s).\n", shown)
}

// printListTable writes the human list table: the heading that names what it
// shows, the table, and the trailing count line.
func printListTable(w io.Writer, c *scopeChoice) error {
	if c.heading != "" {
		terminal.Println(w, c.heading)
	}
	if err := printSessionTable(w, c.rows, c.format); err != nil {
		return err
	}
	printListFooter(w, len(c.rows), c.total, c.truncated, c.format.NarrowHint)
	return nil
}

// createdPRLabel is the pull request a session created last, as "#213", or ""
// when it created none.
func createdPRLabel(m archive.Metadata) string {
	for i := len(m.GitActivity) - 1; i >= 0; i-- {
		if e := m.GitActivity[i]; e.Kind == archive.GitEventPRCreated && e.PRNumber > 0 {
			return "#" + strconv.Itoa(e.PRNumber)
		}
	}
	return ""
}

// withColumns returns opts laid out for rows, and the value each column that
// is left out had, for a heading to name. A PR column appears only when a row
// has a PR, and HARNESS and PROJECT go when every row has the same value (an
// unknown project, "-", is not worth naming). The verbose table keeps every
// column.
func (opts listFormatOptions) withColumns(rows []listRow) (laid listFormatOptions, constants []string) {
	laid = opts
	laid.LiveMarks = slices.ContainsFunc(rows, func(r listRow) bool { return r.Live })
	if opts.Verbose {
		return laid, nil
	}
	laid.ShowPR = slices.ContainsFunc(rows, func(r listRow) bool { return r.PR != "" })
	// One row says nothing by sharing a value with itself.
	if len(rows) < 2 {
		return laid, nil
	}
	same := func(value func(listRow) string) (string, bool) {
		first := value(rows[0])
		return first, !slices.ContainsFunc(rows, func(r listRow) bool { return value(r) != first })
	}
	if project, ok := same(func(r listRow) string { return r.Project }); ok {
		// One name is one heading, even across a repository's checkouts.
		laid.HideProject, laid.GroupByProject = true, false
		if project != "-" {
			constants = append(constants, project)
		}
	}
	if harness, ok := same(func(r listRow) string { return r.Harness }); ok {
		laid.HideHarness = true
		constants = append(constants, harness)
	}
	return laid, constants
}
