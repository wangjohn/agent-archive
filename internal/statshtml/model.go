package statshtml

import "strings"

// page is everything the template shows: text and numbers already formatted
// and every name already cleaned. The template escapes each string for the
// context it lands in; nothing here is marked as trusted markup. The renderer's
// own stylesheet is part of the template's text, not a value.
type page struct {
	CSP   string
	Title string
	// Subtitle is the parts of the line under the title; the template keeps
	// each part whole when the line wraps (a date is never cut in two).
	Subtitle []string
	Filters  string
	// Empty is the message shown instead of the sections when the window has
	// no sessions.
	Empty string

	Daily      *dailyChart
	NoTokens   string // said instead of the chart when no session reports tokens
	Cards      []card
	CardsVs    string
	Agents     *barTable
	Models     *barTable
	Projects   *barTable
	Tokens     *tokenSection
	Highlights []highlight
	Groups     *barTable
	Footer     footer
}

// card is one overview number.
type card struct {
	Label string
	Value string
	// Delta is the change against the previous period as shown ("▲ 18%");
	// DeltaSpoken the same for screen readers.
	Delta       string
	DeltaSpoken string
	Note        string
}

// barTable is a table whose rows may carry a bar: agents, models, projects
// and the --by breakdown.
type barTable struct {
	ID      string
	Title   string
	Heading string // the label column's heading
	Cols    []string
	HasBars bool
	// BarNote says what the bars show; it is hidden with them on a narrow
	// screen.
	BarNote string
	Rows    []barRow
	Notes   []string
}

// barRow is a row of a barTable. Pct is the bar's length as an SVG length.
type barRow struct {
	Label string
	Pct   string
	Cells []string
}

// dailyChart is the tokens-by-day bar chart, drawn in an SVG whose width is
// the page's and whose heights are in pixels, so its text never scales.
type dailyChart struct {
	Height    int
	Baseline  int
	PlotTop   int
	AxisY     int
	MaxWidth  int
	Peak      string // the direct label of the peak bar
	PeakLabel *chartLabel
	Bars      []dayBar
	XLabels   []chartLabel
	Caption   string
	Summary   string // the chart's text alternative
	Rows      []dayRow
}

// chartLabel is a text label at x (an SVG length) and y (pixels).
type chartLabel struct {
	X      string
	Y      string
	Anchor string
	Text   string
}

// dayBar is one bar of the daily chart and the invisible slot that answers
// the pointer for it.
type dayBar struct {
	SlotX string
	SlotW string
	X     string
	W     string
	Y     string
	H     string
	Class string
	Title string
}

// dayRow is one row of the daily chart's table.
type dayRow struct {
	Label    string
	Sessions string
	Tokens   string
}

// tokenSection is what the tokens were spent on: a donut, its legend, and
// the rows that appear when there is something to say.
type tokenSection struct {
	Total     string
	Segments  []segment
	Reasoning string
	Subagents *subagentRow
	Skills    []nameCount
	MCP       []nameCount
	Notes     []string
}

// segment is one part of the donut, with its legend entry.
type segment struct {
	Class  string
	Label  string
	Tokens string
	Share  string
	Dash   string
	Offset string
	// Text is the segment's direct label, shown when the ring has room for it.
	Text    string
	TextX   string
	TextY   string
	HasText bool
	Speak   string
}

type subagentRow struct {
	Text string
	Pct  string
}

type nameCount struct {
	Name  string
	Count string
}

type highlight struct {
	Label string
	Text  string
}

type footer struct {
	Lines []string
	// Privacy says what the page holds and, when project names were replaced,
	// how to show them.
	Privacy   string
	Generated string
}

// letters is the 0-based index as a spreadsheet-style column name: A to Z,
// then AA, AB and so on.
func letters(i int) string {
	var b []byte
	for i++; i > 0; i = (i - 1) / 26 {
		b = append([]byte{byte('A' + (i-1)%26)}, b...)
	}
	return string(b)
}

// namer decides how a name that identifies the user's own work is shown: a
// project, a skill or an MCP server. Unless real names were asked for, each
// distinct name gets a stand-in ("project A", "skill A") in the order the page
// first names it, so one name has one label throughout the file and nothing in
// it can be traced to a client, a repository or an internal tool. Each kind
// of name has its own namer, so the letters of one kind never mix with those
// of another.
type namer struct {
	reveal bool
	noun   string
	labels map[string]string
}

func newNamer(reveal bool, noun string) *namer {
	return &namer{reveal: reveal, noun: noun, labels: map[string]string{}}
}

// name is the label shown for a name read from the archive.
func (n *namer) name(name string) string {
	if n.reveal {
		return clean(name)
	}
	if label, ok := n.labels[name]; ok {
		return label
	}
	label := n.noun + " " + letters(len(n.labels))
	n.labels[name] = label
	return label
}

// project is the label shown for a project name; sessions with none are
// "(no project)", which names nothing.
func (n *namer) project(name string) string {
	if name == "" {
		return "(no project)"
	}
	return n.name(name)
}

// joinSentences joins non-empty sentences with a space.
func joinSentences(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}
