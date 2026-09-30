package cli

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/statshtml"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The three ways to see one run's numbers, the terminal screen, `--json` and
// `--html`, are formatted by separate code (the page has its own copy of the
// terminal's number formatters, because internal/statshtml may not import
// internal/cli). These tests run all three over the same archive and compare
// what each says, section by section, so the copies cannot drift apart.

// crossScenario is an archive for the cross-check and the flags to run it with.
type crossScenario struct {
	name  string
	build func(tb testing.TB, mem *storagetest.MemoryStore)
	args  []string
}

func crossScenarios() []crossScenario {
	only := func(sessions ...syntheticSession) func(testing.TB, *storagetest.MemoryStore) {
		return func(tb testing.TB, mem *storagetest.MemoryStore) {
			tb.Helper()
			for _, s := range sessions {
				s.publish(tb, mem)
			}
		}
	}
	claude := func(id string, captured time.Time, tokens ...modelTokenSpec) syntheticSession {
		return syntheticSession{id: id, harness: "claude", project: "alpha", captured: captured, models: []string{"claude-opus-5"},
			turns: 5, messages: 20, toolResults: 10, errors: 1, perModel: tokens}
	}
	opus := modelTokenSpec{"claude-opus-5", 10_000, 20_000, 300_000, 40_000}
	scenarios := []crossScenario{
		{name: "fixture", build: publishStatsFixture},
		{name: "fixture-by-day", build: publishStatsFixture, args: []string{"--by", "day"}},
		{name: "fixture-by-week", build: publishStatsFixture, args: []string{"--by", "week"}},
		{name: "fixture-by-month", build: publishStatsFixture, args: []string{"--by", "month", "--days", "200"}},
		{name: "fixture-by-project", build: publishStatsFixture, args: []string{"--by", "project"}},
		{name: "fixture-claude-only", build: publishStatsFixture, args: []string{"--harness", "claude"}},
		{name: "fixture-one-day", build: publishStatsFixture, args: []string{"--days", "1"}},
		{name: "fixture-year", build: publishStatsFixture, args: []string{"--days", "400", "--by", "month"}},
		{name: "empty", build: func(testing.TB, *storagetest.MemoryStore) {}},
		{name: "single-session", build: only(claude("one", statsDay(time.September, 20, 10), opus))},
		{name: "cursor-only", build: only(
			syntheticSession{id: "c1", harness: "cursor", project: "alpha", captured: statsDay(time.September, 20, 10), models: []string{"cursor-auto"}, turns: 3, noTokens: true},
			syntheticSession{id: "c2", harness: "cursor", project: "beta", captured: statsDay(time.September, 21, 10), models: []string{"cursor-auto"}, turns: 3, noTokens: true},
		), args: []string{"--by", "project"}},
		{name: "old-parser-approximate-cost", build: only(func() syntheticSession {
			s := claude("old", statsDay(time.September, 20, 10), opus)
			s.parser = "0.13.0"
			return s
		}(), claude("new", statsDay(time.September, 21, 10), opus))},
		{name: "unpriced-model", build: only(
			syntheticSession{id: "u1", harness: "codex", project: "alpha", captured: statsDay(time.September, 20, 10), models: []string{"mystery-1"}, turns: 3,
				perModel: []modelTokenSpec{{"mystery-1", 50_000, 5_000, 20_000, 0}}},
			claude("p1", statsDay(time.September, 21, 10), opus),
		)},
		{name: "only-unpriced", build: only(
			syntheticSession{id: "u1", harness: "codex", project: "alpha", captured: statsDay(time.September, 20, 10), models: []string{"mystery-1"}, turns: 3,
				perModel: []modelTokenSpec{{"mystery-1", 50_000, 5_000, 20_000, 0}}},
		)},
		{name: "nothing-in-the-previous-period", build: only(
			claude("a", statsDay(time.September, 20, 10), opus), claude("b", statsDay(time.September, 21, 10), opus))},
		{name: "zero-tokens", build: only(
			claude("z", statsDay(time.September, 20, 10), modelTokenSpec{"claude-opus-5", 0, 0, 0, 0}))},
		{name: "saturated-sums", build: only(
			claude("s1", statsDay(time.September, 20, 10), modelTokenSpec{"claude-opus-5", math.MaxInt64 / 2, math.MaxInt64 / 2, math.MaxInt64 / 2, 1}),
			claude("s2", statsDay(time.September, 21, 10), modelTokenSpec{"claude-opus-5", math.MaxInt64 / 2, math.MaxInt64 / 2, math.MaxInt64 / 2, 1}),
		)},
		// Totals on the edges of the number formats: 999.6K is 1M, 9,950 is 10K.
		{name: "number-format-edges", build: only(
			claude("e1", statsDay(time.September, 20, 10), modelTokenSpec{"claude-opus-5", 999_600, 9_949, 9_950, 999_499_999}),
			claude("e2", statsDay(time.September, 21, 10), modelTokenSpec{"claude-opus-5", 500, 500, 999_499, 1}))},
		{name: "one-cache-type-only", build: only(
			claude("r", statsDay(time.September, 20, 10), modelTokenSpec{"claude-opus-5", 0, 0, 1_000_000, 0}))},
		{name: "subagents-skills-and-mcp-present", build: func(tb testing.TB, mem *storagetest.MemoryStore) {
			tb.Helper()
			parent := claude("parent", statsDay(time.September, 20, 10), opus)
			parent.skills, parent.mcp = []string{"review-pr"}, map[string]int{"github": 1234}
			child := claude("child", statsDay(time.September, 20, 11), opus)
			child.parent = "parent"
			parent.publish(tb, mem)
			child.publish(tb, mem)
		}},
	}
	return scenarios
}

func TestStatsPageAgreesWithTheTerminalAndJSON(t *testing.T) {
	t.Parallel()
	for _, sc := range crossScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			env, mem := statsEnv(t)
			sc.build(t, mem)
			base := append([]string{"--prices", goldenPrices}, sc.args...)
			jsonOut := mustRunStats(t, env, 0, append([]string{"--json"}, base...)...)
			page := mustRunStats(t, env, 0, append([]string{"--html", "--include-project-names"}, base...)...)
			screen := mustRunStats(t, env, 80, base...)
			wide := mustRunStats(t, env, 120, base...)

			var doc statsDocument
			if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Coverage.Sessions == 0 {
				if !strings.Contains(page, "Nothing to show yet") {
					t.Error("the empty page says nothing is missing")
				}
				return
			}
			// The JSON carries everything the page uses: a page drawn from the
			// document's own stats is the page the command wrote.
			again, err := statshtml.Render(doc.Stats, statshtml.Options{
				GeneratedAt: doc.GeneratedAt, IncludeProjectNames: true,
				Filters: statshtml.Filters{Harness: doc.Filters.Harness, Model: doc.Filters.Model, Origin: doc.Filters.Origin},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, []byte(page)) {
				t.Errorf("the page drawn from the --json document differs from the page the command wrote:\n%s", firstDifference(string(again), page))
			}

			html := parsePage(t, page)
			checkOverview(t, html, screen)
			checkHeader(t, html, screen)
			for _, table := range []struct{ html, terminal string }{
				{"Agents", "AGENTS"}, {"Cost by model", "COST BY MODEL"}, {"Top projects", "TOP PROJECTS"},
			} {
				checkTable(t, html, screen, table.html, table.terminal)
				checkTable(t, html, wide, table.html, table.terminal)
			}
			for _, prefix := range []string{"By day", "By week", "By month", "By project"} {
				if html.section(prefix) != nil {
					checkTable(t, html, screen, prefix, strings.ToUpper(prefix))
				}
			}
			checkComposition(t, html, screen)
			checkHighlights(t, html, screen)
			checkFooter(t, html, screen)
			checkDaily(t, html, doc)
		})
	}
}

// firstDifference names where two pages part.
func firstDifference(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	from := max(i-60, 0)
	return fmt.Sprintf("byte %d:\n  first:  %q\n  second: %q", i, a[from:min(i+80, len(a))], b[from:min(i+80, len(b))])
}

// node is an element of the parsed page.
type node struct {
	name     string
	class    string
	id       string
	attrs    map[string]string
	children []*node
	text     string // the element's own text, in order with its children's
	parts    []any  // string and *node, in document order
}

// parsePage builds the page's element tree; the page is well-formed XHTML.
func parsePage(t *testing.T, page string) *node {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(page))
	root := &node{name: "#root"}
	stack := []*node{root}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the page is not well-formed: %v", err)
		}
		top := stack[len(stack)-1]
		switch tok := tok.(type) {
		case xml.StartElement:
			n := &node{name: tok.Name.Local, attrs: map[string]string{}}
			for _, a := range tok.Attr {
				n.attrs[a.Name.Local] = a.Value
			}
			n.class, n.id = n.attrs["class"], n.attrs["id"]
			top.children = append(top.children, n)
			top.parts = append(top.parts, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			top.parts = append(top.parts, string(tok))
		}
	}
	return root
}

// visible is the text a sighted reader sees: everything but the text kept for
// screen readers, and the stylesheet.
func (n *node) visible() string {
	var b strings.Builder
	var walk func(*node)
	walk = func(n *node) {
		if n.name == "style" || hasClass(n, "sr") {
			return
		}
		for _, p := range n.parts {
			switch p := p.(type) {
			case string:
				b.WriteString(p)
			case *node:
				walk(p)
			}
		}
	}
	walk(n)
	return squash(b.String())
}

func hasClass(n *node, class string) bool {
	for f := range strings.FieldsSeq(n.class) {
		if f == class {
			return true
		}
	}
	return false
}

func (n *node) find(pred func(*node) bool) []*node {
	var out []*node
	var walk func(*node)
	walk = func(m *node) {
		for _, c := range m.children {
			if pred(c) {
				out = append(out, c)
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

func (n *node) byClass(name, class string) []*node {
	return n.find(func(m *node) bool { return m.name == name && hasClass(m, class) })
}

// section is the page's <section> whose heading starts with title.
func (n *node) section(title string) *node {
	for _, s := range n.find(func(m *node) bool { return m.name == "section" }) {
		for _, h := range s.find(func(m *node) bool { return m.name == "h2" }) {
			if strings.HasPrefix(h.visible(), title) {
				return s
			}
		}
	}
	return nil
}

var spaces = regexp.MustCompile(`\s+`)

func squash(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// same normalizes what the two surfaces write differently on purpose: the
// page puts "·" between parts and brackets a share; the screen pads with
// spaces and draws bars.
func same(s string) string {
	s = strings.NewReplacer("·", " ", "(", " ", ")", " ", "\u2011", "-").Replace(s)
	return squash(s)
}

var barOnly = regexp.MustCompile(`^[█▓▒░#=.\-]+$`)

// screenBlock is the screen's block that starts with title (a heading line),
// as its lines.
func screenBlock(screen, title string) []string {
	for block := range strings.SplitSeq(screen, "\n\n") {
		lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
		if strings.HasPrefix(lines[0], title) {
			return lines
		}
	}
	return nil
}

// screenRows are a table block's rows, each as its fields without bars.
func screenRows(lines []string) []string {
	var rows []string
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "+ ") || strings.Contains(line, "not shown") {
			continue
		}
		var fields []string
		for f := range strings.FieldsSeq(line) {
			if !barOnly.MatchString(f) || len(f) < 3 {
				fields = append(fields, f)
			}
		}
		rows = append(rows, same(strings.Join(fields, " ")))
	}
	return rows
}

func checkTable(t *testing.T, page *node, screen, htmlTitle, screenTitle string) {
	t.Helper()
	sec := page.section(htmlTitle)
	block := screenBlock(screen, screenTitle)
	if sec == nil || block == nil {
		if (sec == nil) != (block == nil) {
			t.Errorf("%s: the page has it (%v) but the screen does not (%v)", htmlTitle, sec != nil, block != nil)
		}
		return
	}
	var want []string
	for _, tr := range sec.find(func(m *node) bool { return m.name == "tr" }) {
		if len(tr.children) > 0 && tr.children[0].name == "th" && tr.children[0].attrs["scope"] == "row" {
			var cells []string
			for _, c := range tr.children {
				if hasClass(c, "barcol") {
					continue
				}
				cells = append(cells, c.visible())
			}
			want = append(want, same(strings.Join(cells, " ")))
		}
	}
	got := screenRows(block)
	// The compact screen has a share column the wide one has as a bar, and
	// the page has both: compare what the screen has, cell by cell.
	if len(got) != len(want) {
		t.Errorf("%s: the page has %d rows, the screen %d\npage:   %q\nscreen: %q", htmlTitle, len(want), len(got), want, got)
		return
	}
	for i := range got {
		if !sameCells(want[i], got[i]) {
			t.Errorf("%s row %d: page %q, screen %q", htmlTitle, i, want[i], got[i])
		}
	}
}

// sameCells is whether every field of the screen's row is in the page's, in
// order: the page may carry a cell the narrow screen leaves to a bar.
func sameCells(page, screen string) bool {
	rest := strings.Fields(page)
	for _, f := range strings.Fields(screen) {
		found := false
		for len(rest) > 0 {
			head := rest[0]
			rest = rest[1:]
			if head == f {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func checkOverview(t *testing.T, page *node, screen string) {
	t.Helper()
	block := screenBlock(screen, "OVERVIEW")
	if block == nil {
		t.Fatal("the screen has no overview")
	}
	cards := page.byClass("div", "stat")
	if len(cards) != len(block)-1 {
		t.Fatalf("the page has %d overview cards, the screen %d rows", len(cards), len(block)-1)
	}
	for i, card := range cards {
		var fields []string
		for _, c := range card.children {
			// The "at list price" note is on the wide screen only.
			if hasClass(c, "note") && c.visible() == "at list price" {
				continue
			}
			fields = append(fields, c.visible())
		}
		got := squash(strings.Join(fields, " "))
		want := squash(strings.ReplaceAll(block[i+1], "at list price", ""))
		if got != want {
			t.Errorf("overview row %d: page %q, screen %q", i, got, want)
		}
	}
}

func checkHeader(t *testing.T, page *node, screen string) {
	t.Helper()
	sub := page.byClass("p", "sub")[0].visible()
	first := screenBlock(screen, "agent-archive stats")
	if first == nil {
		t.Fatal("the screen has no heading")
	}
	// The screen's heading may wrap over lines, and the page adds the window's
	// dates and puts its filters on a line of their own.
	head := same(strings.Join(first, " "))
	tail := regexp.MustCompile(`\d+ agents? .*$`).FindString(head)
	if tail == "" || !strings.Contains(same(sub), tail) {
		t.Errorf("the page's subtitle %q does not say %q, as the screen's heading does", sub, tail)
	}
	subs := page.byClass("p", "sub")
	filters := same(subs[len(subs)-1].visible())
	for _, filter := range []string{"harness", "model"} {
		if _, value, ok := strings.Cut(head, " "+filter+" "); ok {
			if want := filter + " " + strings.Fields(value)[0]; !strings.Contains(filters, want) {
				t.Errorf("the page does not say it is filtered to %q: %q", want, filters)
			}
		}
	}
}

func checkComposition(t *testing.T, page *node, screen string) {
	t.Helper()
	sec := page.section("What used your tokens")
	block := screenBlock(screen, "WHAT USED YOUR TOKENS")
	if sec == nil || block == nil {
		if (sec == nil) != (block == nil) {
			t.Fatalf("composition: page has it %v, screen %v", sec != nil, block != nil)
		}
		return
	}
	legend := sec.byClass("table", "legend")[0]
	var want []string
	for _, tr := range legend.find(func(m *node) bool { return m.name == "tr" }) {
		if len(tr.children) > 0 && tr.children[0].name == "th" && tr.children[0].attrs["scope"] == "row" {
			var cells []string
			for _, c := range tr.children {
				cells = append(cells, c.visible())
			}
			want = append(want, strings.Join(cells, " "))
		}
	}
	var got []string
	// The legend is the four lines after the heading, each led by its glyph.
	for _, line := range block[1:min(len(block), 5)] {
		got = append(got, strings.Join(strings.Fields(line)[1:], " "))
	}
	if len(got) != 4 || len(want) != 4 {
		t.Fatalf("composition legend: page %q, screen %q from %q", want, got, block)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("composition legend row %d: page %q, screen %q", i, want[i], got[i])
		}
	}
	text := squash(strings.Join(block, " "))
	for _, row := range sec.byClass("div", "row") {
		body := row.visible()
		label := strings.SplitN(body, " ", 2)[0]
		rest := strings.TrimSpace(strings.TrimPrefix(body, label))
		switch label {
		case "Subagents":
			if !strings.Contains(text, rest) {
				t.Errorf("subagents: page %q, screen %q", rest, text)
			}
		case "Skills", "MCP":
			if a, b := chipCounts(rest), screenChips(text, label); !equalCounts(a, b) {
				t.Errorf("%s: page %v, screen %v", label, a, b)
			}
		}
	}
	for _, note := range sec.byClass("p", "note") {
		if !strings.Contains(text, note.visible()) {
			t.Errorf("composition note %q is not on the screen: %q", note.visible(), text)
		}
	}
}

// chipCounts reads "review-pr 13 sessions create-skill 1 session" as counts.
func chipCounts(text string) map[string]int {
	out := map[string]int{}
	fields := strings.Fields(text)
	for i := 0; i+1 < len(fields); {
		n, err := strconv.Atoi(strings.ReplaceAll(fields[i+1], ",", ""))
		if err != nil {
			i++
			continue
		}
		out[fields[i]] = n
		i += 3
	}
	return out
}

// screenChips reads "Skills review-pr 13 · create-skill 1" from the screen.
func screenChips(text, label string) map[string]int {
	out := map[string]int{}
	_, rest, ok := strings.Cut(text, label+" ")
	if !ok {
		return out
	}
	end := len(rest)
	for _, stop := range []string{" Skills counts", " MCP counts", " MCP:", " Skills count", " MCP "} {
		if i := strings.Index(rest, stop); i >= 0 && i < end {
			end = i
		}
	}
	for item := range strings.SplitSeq(rest[:end], " · ") {
		f := strings.Fields(item)
		if len(f) != 2 {
			continue
		}
		if n, err := strconv.Atoi(strings.ReplaceAll(f[1], ",", "")); err == nil {
			out[f[0]] = n
		}
	}
	return out
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func checkHighlights(t *testing.T, page *node, screen string) {
	t.Helper()
	sec := page.section("Highlights")
	block := screenBlock(screen, "HIGHLIGHTS")
	if sec == nil || block == nil {
		if (sec == nil) != (block == nil) {
			t.Fatalf("highlights: page has it %v, screen %v", sec != nil, block != nil)
		}
		return
	}
	text := same(strings.Join(block[1:], " "))
	for _, dd := range sec.find(func(m *node) bool { return m.name == "dd" }) {
		if want := same(dd.visible()); !strings.Contains(text, want) {
			t.Errorf("highlight %q is not on the screen:\n%s", want, text)
		}
	}
}

func checkFooter(t *testing.T, page *node, screen string) {
	t.Helper()
	foot := page.find(func(m *node) bool { return m.name == "footer" })[0].visible()
	text := squash(strings.Join(screenBlock(screen, "Scope:"), " "))
	// Every sentence about scope, prices and qualifiers the screen makes is on
	// the page too (apart from the flag it names differently).
	for _, sentence := range []string{"Scope: this archive only.", "Cost is an estimate at list price, not a bill."} {
		if strings.Contains(text, sentence) != strings.Contains(foot, sentence) {
			t.Errorf("the footers disagree about %q", sentence)
		}
	}
	for _, re := range []string{
		`Sessions with no token data are left out of token and cost totals \([^)]*\)\.`,
		`Prices [^,]*, as of [0-9-]+`,
		`~ priced at the session's main model for \d+ sessions? with no per-model split`,
		`\+ leaves out tokens of models the price table does not list( \([^)]*\))?\.`,
	} {
		onScreen := regexp.MustCompile(re).FindString(text)
		onPage := regexp.MustCompile(re).FindString(foot)
		if onScreen != onPage {
			t.Errorf("footers differ for %q:\n screen: %q\n page:   %q", re, onScreen, onPage)
		}
	}
}

// checkDaily compares the chart's table with the daily series in the JSON.
func checkDaily(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	sec := page.section("Tokens by day")
	if sec == nil {
		t.Error("the page has no daily chart section")
		return
	}
	if doc.Coverage.SessionsWithTokens == 0 {
		if !strings.Contains(sec.visible(), "No session in this window reports token counts") || len(sec.byClass("table", "")) > 0 {
			t.Errorf("a window without token data draws a chart: %q", sec.visible())
		}
		return
	}
	var rows [][]string
	for _, tr := range sec.find(func(m *node) bool { return m.name == "tr" }) {
		if len(tr.children) > 0 && tr.children[0].attrs["scope"] == "row" {
			var cells []string
			for _, c := range tr.children {
				cells = append(cells, c.visible())
			}
			rows = append(rows, cells)
		}
	}
	if len(doc.Daily) > 120 {
		return // runs of days; the budget test covers them
	}
	if len(rows) != len(doc.Daily) {
		t.Fatalf("the chart table has %d rows for %d days", len(rows), len(doc.Daily))
	}
	var peak int64
	for i, d := range doc.Daily {
		when, _ := time.Parse("2006-01-02", d.Date)
		label := when.Format("Jan 2")
		if doc.Window.Days > 300 {
			label = when.Format("Jan 2 2006")
		}
		tokens := "0"
		switch {
		case d.Tokens == nil && d.Sessions > 0:
			tokens = "unknown"
		case d.Tokens != nil:
			tokens = tokenCount(*d.Tokens)
			peak = max(peak, *d.Tokens)
		}
		want := []string{label, commaInt(int64(d.Sessions)), tokens}
		if strings.Join(rows[i], "|") != strings.Join(want, "|") {
			t.Errorf("day %s: chart table %q, JSON says %q", d.Date, rows[i], want)
		}
	}
	if doc.Peak != nil {
		if doc.Peak.Tokens != peak {
			t.Errorf("JSON peak %d is not the busiest day's %d", doc.Peak.Tokens, peak)
		}
		if peak > 0 && !strings.Contains(squash(fmt.Sprint(sec.byClass("text", "peak-label")[0].visible())), "Peak "+tokenCount(peak)) {
			t.Errorf("the chart's peak label does not say %s", tokenCount(peak))
		}
	}
}
