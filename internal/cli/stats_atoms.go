package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// Text that names a command, or a name with its count, is wrapped by atoms:
// pieces that stay on one line whenever they fit. A line break lands between
// atoms, not inside one, so "--json --by project" is never cut after "--by"
// and "github 41" never after "github". An atom wider than the room there is
// falls back to its words, so nothing is ever wider than the screen.

// packAtoms lays atoms out in lines, one space apart, starting at column
// first, with following lines indented; a line is never wider than the
// screen. An atom wider than the room on a line is split into its words.
func (p *statsPrinter) packAtoms(first, indent int, atoms []string) []string {
	room := p.width - indent
	fitted := make([]string, 0, len(atoms))
	for _, atom := range atoms {
		if visibleWidth(atom) > room {
			fitted = append(fitted, strings.Fields(atom)...)
			continue
		}
		fitted = append(fitted, atom)
	}
	return packItems(fitted, first, indent, 1, p.width)
}

// hangAtoms is hang for text made of atoms: the label, then the atoms, the
// following lines indented under the first. The label is padded to labelW and
// styled by paint.
func (p *statsPrinter) hangAtoms(label string, labelW int, atoms []string, paint func(string) string) []string {
	start := labelW + 2
	lines := p.packAtoms(start, start, atoms)
	lines[0] = paint(padRight(label, labelW)) + "  " + lines[0]
	return lines
}

// commandAtoms are the pieces a command is wrapped by: the whole command when
// it fits in room columns, else its lead (the program and what it is given
// before the flags) and its flags, each one atom. suffix, a closing
// parenthesis or a comma, stays with the last atom. lead may be empty.
func commandAtoms(lead, flags, suffix string, room int) []string {
	if lead == "" {
		return []string{flags + suffix}
	}
	if visibleWidth(lead+" "+flags+suffix) <= room {
		return []string{lead + " " + flags + suffix}
	}
	return []string{lead, flags + suffix}
}

// allInAtoms is "head (all in COMMAND)", the hint under a list a screen cuts.
func allInAtoms(head string, command string, room int) []string {
	atoms := strings.Fields(head)
	atoms = append(atoms, "(all", "in")
	return append(atoms, commandAtoms("", command, ")", room)...)
}

// moreAtoms is the line under a list that leaves n rows out, and where to
// find them all: command is the flags that list every row. The interactive
// screen takes no command, and its window is the one w chose, not a flag, so
// there the line says to quit first and names the window (and that the
// filters it was started with apply). room is the columns an atom may fill
// before it is split.
func (p *statsPrinter) moreAtoms(n int, command string, room int) []string {
	head := "+ " + strconv.Itoa(n) + " more"
	if !p.v.interactive {
		return allInAtoms(head, command, room)
	}
	atoms := strings.Fields(head + " (quit, then run")
	lead := fmt.Sprintf("agent-archive stats --days %d", p.s.Window.Days)
	if p.v.filters == (statsFilters{}) {
		return append(atoms, commandAtoms(lead, command, ")", room)...)
	}
	atoms = append(atoms, commandAtoms(lead, command, ",", room)...)
	return append(atoms, "with", "the", "same", "filters)")
}

// usageAtoms are the atoms of a row of used names and their counts: each
// "name count" is one atom, the separator stays at the end of the line it
// follows, and the unit ("sessions", "calls") comes last.
func (p *statsPrinter) usageAtoms(items []string, unit string) []string {
	atoms := make([]string, 0, len(items)+1)
	for i, item := range items {
		if i < len(items)-1 {
			item += " " + p.g.sep
		}
		atoms = append(atoms, item)
	}
	return append(atoms, unit)
}

// endWith adds suffix to the last atom.
func endWith(atoms []string, suffix string) []string {
	atoms[len(atoms)-1] += suffix
	return atoms
}
