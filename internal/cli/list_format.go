package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
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
}

// listFormatOptions controls how session rows are built and printed.
type listFormatOptions struct {
	Now      time.Time
	Verbose  bool
	Numbered bool
	Projects map[string]string // project_id → display label (basename)
	Style    textStyle
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
			Index:      i + 1,
			SessionID:  m.SessionID,
			ShortID:    shorts[i],
			HarnessKey: m.Harness.Name,
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

// printSessionTable writes the human list table for rows.
func printSessionTable(w io.Writer, rows []listRow, opts listFormatOptions) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if opts.Verbose {
		if opts.Numbered {
			terminal.Println(tw, "#\tTITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
		} else {
			terminal.Println(tw, "TITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
		}
		for _, r := range rows {
			if opts.Numbered {
				terminal.Printf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Index, r.Title, archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
			} else {
				terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Title, archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
			}
		}
	} else {
		if opts.Numbered {
			terminal.Println(tw, "#\tTITLE\tWHEN\tHARNESS\tPROJECT\tID")
		} else {
			terminal.Println(tw, "TITLE\tWHEN\tHARNESS\tPROJECT\tID")
		}
		for _, r := range rows {
			title := r.Title
			if r.SkillHint != "" {
				if opts.Style.color {
					title += opts.Style.dim(r.SkillHint)
				} else {
					title += r.SkillHint
				}
			}
			if opts.Numbered {
				terminal.Printf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
					r.Index, title, r.When, r.Harness, r.Project, archive.DisplayLine(r.ShortID))
			} else {
				terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\n",
					title, r.When, r.Harness, r.Project, archive.DisplayLine(r.ShortID))
			}
		}
	}
	return tw.Flush()
}

// printListFooter writes the trailing count / truncation line.
func printListFooter(w io.Writer, shown, totalMatched int, truncated bool) {
	if truncated {
		terminal.Printf(w, "Showing %d of %d session(s). Use --limit 0 for all, or narrow with --since / --harness.\n", shown, totalMatched)
		return
	}
	terminal.Printf(w, "%d session(s).\n", shown)
}

// printListTable writes the human list table and trailing count line.
func printListTable(w io.Writer, sessions []archive.Metadata, totalMatched int, truncated bool, opts listFormatOptions) error {
	rows := formatSessionRows(sessions, opts)
	if err := printSessionTable(w, rows, opts); err != nil {
		return err
	}
	printListFooter(w, len(sessions), totalMatched, truncated)
	return nil
}
