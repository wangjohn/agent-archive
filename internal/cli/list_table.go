package cli

import (
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printSessionTable writes the human list table for rows. When GroupByProject
// is set and more than one project appears, rows are printed under project
// headings (indices stay global for the interactive picker).
func printSessionTable(w io.Writer, rows []listRow, opts listFormatOptions) error {
	return printSessionGroups(w, sessionTableGroups(rows, opts), opts)
}

// sessionTableGroup is the rows under one project heading of the human list
// table. The flat table is a single group without a heading.
type sessionTableGroup struct {
	// label is the project's heading, styled; empty for the flat table.
	label string
	// count is how many rows the whole group has.
	count int
	rows  []listRow
	// continued marks a group whose earlier rows are on a previous page.
	continued bool
}

// sessionTableGroups arranges rows as printSessionTable prints them. Their
// rows, taken in order, are the table's rows from top to bottom.
func sessionTableGroups(rows []listRow, opts listFormatOptions) []sessionTableGroup {
	if !opts.GroupByProject || distinctProjects(rows) <= 1 {
		return []sessionTableGroup{{count: len(rows), rows: rows}}
	}
	order := make([]listRow, 0)
	seen := map[string]bool{}
	labelGroups := map[string]int{}
	for _, r := range rows {
		key := projectGroupKey(r)
		if !seen[key] {
			seen[key] = true
			order = append(order, r)
			labelGroups[r.Project]++
		}
	}
	projectIDs := make([]string, len(order))
	for i, group := range order {
		projectIDs[i] = group.ProjectID
	}
	shortIDs := uniqueShortIDs(projectIDs)
	groups := make([]sessionTableGroup, 0, len(order))
	for i, group := range order {
		label := group.Project
		if label == "-" {
			label = "unknown project"
		}
		if labelGroups[group.Project] > 1 {
			if group.ProjectID != "" {
				label += " [" + archive.DisplayLine(shortIDs[i]) + "]"
			} else {
				label += " [unidentified]"
			}
		}
		if opts.Style.color {
			label = opts.Style.bold(label)
		}
		var members []listRow
		for _, r := range rows {
			if projectGroupKey(r) == projectGroupKey(group) {
				members = append(members, r)
			}
		}
		groups = append(groups, sessionTableGroup{label: label, count: len(members), rows: members})
	}
	return groups
}

// printSessionGroups writes groups as the human list table: each group's
// heading, when it has one, then its column header and rows.
func printSessionGroups(w io.Writer, groups []sessionTableGroup, opts listFormatOptions) error {
	for i, group := range groups {
		if group.label != "" {
			if i > 0 {
				terminal.Println(w)
			}
			if group.continued {
				terminal.Printf(w, "%s (continued)\n", group.label)
			} else {
				terminal.Printf(w, "%s (%d)\n", group.label, group.count)
			}
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		printSessionHeader(tw, opts)
		for _, r := range group.rows {
			printSessionRow(tw, r, opts)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func distinctProjects(rows []listRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[projectGroupKey(r)] = true
	}
	return len(seen)
}

func projectGroupKey(row listRow) string {
	if row.ProjectID != "" {
		return "id:" + row.ProjectID
	}
	return "name:" + row.Project
}

func printSessionHeader(tw *tabwriter.Writer, opts listFormatOptions) {
	if opts.Verbose {
		if opts.Numbered {
			terminal.Println(tw, "#\tTITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
			return
		}
		terminal.Println(tw, "TITLE\tSESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED\tPROJECT")
		return
	}
	terminal.Println(tw, strings.Join(tableHeader(opts), "\t"))
}

// tableHeader is the column names of the plain table, left out where opts
// hides a column.
func tableHeader(opts listFormatOptions) []string {
	var cols []string
	if opts.Numbered {
		cols = append(cols, "#")
	}
	cols = append(cols, "TITLE")
	if opts.ShowPR {
		cols = append(cols, "PR")
	}
	cols = append(cols, "WHEN")
	if !opts.HideHarness {
		cols = append(cols, "HARNESS")
	}
	if !opts.HideProject {
		cols = append(cols, "PROJECT")
	}
	return append(cols, "ID")
}

// liveMark leads the title of a session active right now.
const liveMark = "●"

// tableCells is one row of the plain table, in tableHeader's columns.
func tableCells(r listRow, opts listFormatOptions) []string {
	title := r.Title
	if opts.LiveMarks {
		// Room for the dot on every row, so titles stay aligned.
		if r.Live {
			title = liveMark + " " + title
		} else {
			title = "  " + title
		}
	}
	if r.SkillHint != "" {
		if opts.Style.color {
			title += opts.Style.dim(r.SkillHint)
		} else {
			title += r.SkillHint
		}
	}
	id := archive.DisplayLine(r.ShortID)
	if opts.DimID && opts.Style.color {
		id = opts.Style.dim(id)
	}
	var cells []string
	if opts.Numbered {
		cells = append(cells, strconv.Itoa(r.Index))
	}
	cells = append(cells, title)
	if opts.ShowPR {
		cells = append(cells, r.PR)
	}
	cells = append(cells, r.When)
	if !opts.HideHarness {
		cells = append(cells, r.Harness)
	}
	if !opts.HideProject {
		cells = append(cells, r.Project)
	}
	return append(cells, id)
}

func printSessionRow(tw *tabwriter.Writer, r listRow, opts listFormatOptions) {
	if opts.Verbose {
		if opts.Numbered {
			terminal.Printf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Index, r.Title, archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
			return
		}
		terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Title, archive.DisplayLine(r.SessionID), r.Harness, r.CapturedAt, r.Origin, r.Parser, r.ModelAll, r.Skills, r.Project)
		return
	}
	terminal.Println(tw, strings.Join(tableCells(r, opts), "\t"))
}
