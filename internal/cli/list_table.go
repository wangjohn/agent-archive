package cli

import (
	"io"
	"text/tabwriter"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printSessionTable writes the human list table for rows. When GroupByProject
// is set and more than one project appears, rows are printed under project
// headings (indices stay global for the interactive picker).
func printSessionTable(w io.Writer, rows []listRow, opts listFormatOptions) error {
	if opts.GroupByProject && distinctProjects(rows) > 1 {
		return printSessionTableGrouped(w, rows, opts)
	}
	return printSessionTableFlat(w, rows, opts)
}

func distinctProjects(rows []listRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Project] = true
	}
	return len(seen)
}

func printSessionTableFlat(w io.Writer, rows []listRow, opts listFormatOptions) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	printSessionHeader(tw, opts)
	for _, r := range rows {
		printSessionRow(tw, r, opts)
	}
	return tw.Flush()
}

func printSessionTableGrouped(w io.Writer, rows []listRow, opts listFormatOptions) error {
	order := make([]string, 0)
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Project] {
			seen[r.Project] = true
			order = append(order, r.Project)
		}
	}
	for i, project := range order {
		if i > 0 {
			terminal.Println(w)
		}
		label := project
		if label == "-" {
			label = "unknown project"
		}
		heading := label
		if opts.Style.color {
			heading = opts.Style.bold(label)
		}
		count := 0
		for _, r := range rows {
			if r.Project == project {
				count++
			}
		}
		terminal.Printf(w, "%s (%d)\n", heading, count)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		printSessionHeader(tw, opts)
		for _, r := range rows {
			if r.Project != project {
				continue
			}
			printSessionRow(tw, r, opts)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
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
	if opts.Numbered {
		terminal.Println(tw, "#\tTITLE\tWHEN\tHARNESS\tPROJECT\tID")
		return
	}
	terminal.Println(tw, "TITLE\tWHEN\tHARNESS\tPROJECT\tID")
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
		return
	}
	terminal.Printf(tw, "%s\t%s\t%s\t%s\t%s\n",
		title, r.When, r.Harness, r.Project, archive.DisplayLine(r.ShortID))
}
