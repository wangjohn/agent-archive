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
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/statsfmt"
	"github.com/wangjohn/agent-archive/internal/statshtml"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The web page and `--json` are laid out by separate code (the page shares only
// the number formatters in internal/statsfmt, because internal/statshtml may
// not import internal/cli). These tests run both over the same archive and
// compare what each says. The terminal screens are checked against the JSON in
// stats_pages_check_test.go.

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

func TestStatsPageAgreesWithJSON(t *testing.T) {
	t.Parallel()
	for _, sc := range crossScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			env, mem := statsEnv(t)
			sc.build(t, mem)
			base := append([]string{"--prices", goldenPrices}, sc.args...)
			jsonOut := mustRunStats(t, env, 0, append([]string{"--json"}, base...)...)
			page := mustRunStats(t, env, 0, append([]string{"--html", "--include-names"}, base...)...)

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
				GeneratedAt: doc.GeneratedAt, IncludeNames: true,
				Filters: statshtml.Filters{Harness: doc.Filters.Harness, Model: doc.Filters.Model, Origin: doc.Filters.Origin},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, []byte(page)) {
				t.Errorf("the page drawn from the --json document differs from the page the command wrote:\n%s", firstDifference(string(again), page))
			}

			html := parsePage(t, page)
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
	parts    []any // string and *node, in document order
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
			tokens = statsfmt.TokenCount(*d.Tokens)
			peak = max(peak, *d.Tokens)
		}
		want := []string{label, statsfmt.CommaInt(int64(d.Sessions)), tokens}
		if strings.Join(rows[i], "|") != strings.Join(want, "|") {
			t.Errorf("day %s: chart table %q, JSON says %q", d.Date, rows[i], want)
		}
	}
	if doc.Peak != nil {
		if doc.Peak.Tokens != peak {
			t.Errorf("JSON peak %d is not the busiest day's %d", doc.Peak.Tokens, peak)
		}
		if peak > 0 && !strings.Contains(squash(sec.byClass("text", "peak-label")[0].visible()), "Peak "+statsfmt.TokenCount(peak)) {
			t.Errorf("the chart's peak label does not say %s", statsfmt.TokenCount(peak))
		}
	}
}
