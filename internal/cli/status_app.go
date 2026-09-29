package cli

import (
	"io"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printAppStatus writes status APP: the overall state, then one app in
// full: its line, every session it is uploading, its gaps, and a table of
// its projects. It returns the command's exit code: 1 when the app is not
// one this installation captures or imported.
func printAppStatus(out, errOut io.Writer, view statusView, name string, sc statusScreen) int {
	s := sc.style
	if !view.configured {
		sc.printHeader(out, view)
		return 0
	}
	var row statusRow
	var app appStatus
	found := false
	for _, candidate := range view.Apps {
		if candidate.Name == name {
			app, row, found = candidate, sc.appRow(candidate, 0), true
		}
	}
	for _, candidate := range view.importedApps {
		if candidate.Name == name && !found {
			app, row, found = candidate, sc.importedAppRow(candidate, 0), true
		}
	}
	if !found {
		captured := make([]string, 0, len(view.Apps))
		for _, other := range view.Apps {
			captured = append(captured, other.Name)
		}
		terminal.Printf(errOut, "agent-archive: status: %s isn't one of the apps this installation captures (%s). Run agent-archive setup to add it.\n", appName(name), friendlyApps(captured))
		return 1
	}
	sc.printHeader(out, view)
	terminal.Printf(out, "\n%s\n", s.bold("Capture"))
	sc.printRows(out, []statusRow{row})
	if table := sc.projectTable(app); len(table) > 0 {
		terminal.Printf(out, "\n%s\n", s.bold("Projects"))
		for _, line := range table {
			terminal.Println(out, line)
		}
	}
	sc.printNotes(out, view)
	if sc.verbose {
		terminal.Printf(out, "\n%s\n", s.bold("Details"))
		printAppDetails(out, app)
	}
	terminal.Println(out)
	if sc.verbose {
		terminal.Printf(out, "%s %s\n", s.dim("As JSON:"), s.cmd("agent-archive status --json"))
		return 0
	}
	sc.printFooter(out, "agent-archive status", "agent-archive status "+name+" --verbose")
	return 0
}

// projectTable is status APP's table of an app's projects: for each, the
// sessions its hooks captured, those imported, those uploading, and how far
// read-back has got. A project no configured pair owns (a configuration
// without projects) has no read-back of its own.
func (sc statusScreen) projectTable(app appStatus) []string {
	rows := [][]string{{"Project", "Sessions", "Imported", "Uploading", "Read-back"}}
	for _, pair := range app.Projects {
		rows = append(rows, []string{sc.path(pair.ProjectRoot), strconv.Itoa(pair.sessions), strconv.Itoa(pair.imported), strconv.Itoa(pair.uploading), pairProgress(pair)})
	}
	for _, other := range app.otherProjects {
		rows = append(rows, []string{sc.path(other.ProjectRoot), strconv.Itoa(other.sessions), strconv.Itoa(other.imported), strconv.Itoa(other.uploading), "-"})
	}
	if len(rows) == 1 {
		return nil
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		var line strings.Builder
		line.WriteString("  ")
		for j, cell := range row {
			if j == len(row)-1 {
				line.WriteString(cell)
				break
			}
			line.WriteString(cell + strings.Repeat(" ", widths[j]-visibleWidth(cell)+3))
		}
		text := line.String()
		if i == 0 {
			text = "  " + sc.style.dim(strings.TrimPrefix(text, "  "))
		}
		lines = append(lines, text)
	}
	return lines
}
