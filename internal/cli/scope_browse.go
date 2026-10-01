package cli

import (
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// scopeView is the rows one scope shows, before they are laid out.
type scopeView struct {
	rows []listRow
	// total counts what the scope holds, or is -1 when it was not read to
	// the end; truncated is set when rows is only the first part.
	total     int
	truncated bool
	// hidden is how many subagent sessions the view leaves out, which its
	// footer says.
	hidden int
	// note is a line the footer adds (a search's count of matches elsewhere).
	note string
}

// scopeRowsFunc builds the rows a scope shows. A scope with All set is every
// session.
type scopeRowsFunc func(scope sessionScope) scopeView

// scopeChoice is one thing the browser can show: a scope's rows, laid out,
// under a heading that names them.
type scopeChoice struct {
	scopeView
	format  listFormatOptions
	heading string
	// constants are the values of the columns left out, which the heading
	// names.
	constants []string
}

// scopeChoices is what a browser shows of the archive: the working
// directory's scope, or all projects, and which one now. `a` switches between
// the two. The other one is built only when it is asked for.
type scopeChoices struct {
	scope   sessionScope
	rowsFor scopeRowsFunc
	// format is the table's options before they are laid out for the rows.
	format listFormatOptions
	built  [2]*scopeChoice
	// current is viewScope or viewAll.
	current int
	// fellBack is set when the scope held nothing, so all projects are shown
	// with no scope to go back to.
	fellBack bool
	// plain is set for output with no keys to press: the heading names the
	// flag that widens the scope instead of the key.
	plain bool
}

const (
	viewScope = 0
	viewAll   = 1
)

// newScopeChoices opens on the scope, on all projects when there is none or
// it was turned off (--all-projects), and on all projects too, saying so,
// when the scope holds nothing.
func newScopeChoices(scope sessionScope, format listFormatOptions, plain bool, rowsFor scopeRowsFunc) *scopeChoices {
	c := &scopeChoices{scope: scope.only(), rowsFor: rowsFor, format: format, plain: plain}
	switch {
	case scope.Label == "" || scope.All:
		c.current = viewAll
	case len(c.choice(viewScope).rows) == 0:
		c.current, c.fellBack = viewAll, true
	default:
		c.current = viewScope
	}
	return c
}

// shown is the choice on screen.
func (c *scopeChoices) shown() *scopeChoice { return c.choice(c.current) }

// canToggle reports whether `a` has another choice to show.
func (c *scopeChoices) canToggle() bool { return c.scope.Label != "" && !c.fellBack }

// toggle shows the other choice. A scope that holds nothing (all projects
// were shown first, with --all-projects) is not shown empty: all projects
// stay, and the heading says so, as when the browser opens on such a scope.
func (c *scopeChoices) toggle() {
	if !c.canToggle() {
		return
	}
	if c.current == viewAll && len(c.choice(viewScope).rows) == 0 {
		c.fellBack = true
		all := c.choice(viewAll)
		all.heading = c.heading(viewAll, all.scopeView, all.constants)
		return
	}
	c.current = 1 - c.current
}

// choice builds choice i once.
func (c *scopeChoices) choice(i int) *scopeChoice {
	if built := c.built[i]; built != nil {
		return built
	}
	scope := c.scope
	if i == viewAll {
		scope = scope.everything()
	}
	view := c.rowsFor(scope)
	format, constants := c.format.withColumns(view.rows)
	format.HiddenSubagents, format.Note = view.hidden, view.note
	c.built[i] = &scopeChoice{scopeView: view, format: format, heading: c.heading(i, view, constants), constants: constants}
	return c.built[i]
}

// heading is the line above the table, naming what is shown and the other
// choice: "agent-archive · 42 sessions · a all projects", or "All projects ·
// 104 sessions · a agent-archive". Columns left out because every row has one
// value are named here instead. With no scope, and nothing left out, there is
// nothing to say.
func (c *scopeChoices) heading(i int, view scopeView, constants []string) string {
	// A folder's or a project's name, cleaned like the PROJECT column.
	label := archive.DisplayLine(c.scope.Label)
	var parts []string
	switch {
	case label == "":
	case c.fellBack:
		parts = append(parts, "Nothing in "+label, "showing all projects")
	case i == viewScope:
		parts = append(parts, label)
	default:
		parts = append(parts, allProjectsLabel)
	}
	if len(parts) == 0 && len(constants) == 0 {
		return ""
	}
	if view.total >= 0 {
		parts = append(parts, plural(view.total, "session"))
	} else {
		parts = append(parts, fmt.Sprintf("%d+ sessions", len(view.rows)))
	}
	for _, value := range constants {
		// The scope's label already names the project.
		if !strings.EqualFold(value, label) || i == viewAll {
			parts = append(parts, value)
		}
	}
	switch {
	case !c.canToggle():
	case c.plain && i == viewScope:
		parts = append(parts, "--all-projects lists every project")
	case c.plain:
	case i == viewScope:
		parts = append(parts, "a "+strings.ToLower(allProjectsLabel))
	default:
		parts = append(parts, "a "+label)
	}
	return strings.Join(parts, " · ")
}
