package cli

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// The checks of TestStatsPageAgreesWithTheJSON: the page, parsed, against the
// numbers of the --json document it was drawn from, section by section. The
// expectations here are written out from the document's fields, not read back
// from the renderer.

// maxDailyBars is how many days the page draws one bar each for; a longer window
// draws runs of days, which the budget test in internal/statshtml covers.
const maxDailyBars = 120

// pageCard is the card of the page whose heading has this id.
func pageCard(page *node, headingID string) *node {
	for _, s := range page.find(func(m *node) bool { return m.name == "section" }) {
		if s.attrs["aria-labelledby"] == headingID {
			return s
		}
	}
	return nil
}

// rowsOf are a table's body rows, each as its cells' visible text, without the
// column that holds a bar.
func rowsOf(table *node) [][]string {
	var rows [][]string
	for _, tr := range table.find(func(m *node) bool { return m.name == "tr" }) {
		if len(tr.children) == 0 || tr.children[0].name != "th" || tr.children[0].attrs["scope"] != "row" {
			continue
		}
		var cells []string
		for _, c := range tr.children {
			if !hasClass(c, "barcol") {
				cells = append(cells, c.visible())
			}
		}
		rows = append(rows, cells)
	}
	return rows
}

// jsonCost is a cost as the page shows it: the amount, a ~ before it when it is
// approximate and a + after it when it leaves out unpriced tokens.
func jsonCost(currency string, c stats.Cost, precise bool) string {
	if c.USD == nil {
		return "unpriced"
	}
	text := statsfmt.Money(currency, *c.USD, precise)
	if c.Approximate {
		text = "~" + text
	}
	if c.Partial {
		text += "+"
	}
	return text
}

func spendDown(c stats.Cost) (float64, bool) {
	if c.USD == nil || math.IsNaN(*c.USD) || math.IsInf(*c.USD, 0) || *c.USD < 0 {
		return 0, false
	}
	return *c.USD, true
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func checkPageAgainstJSON(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	checkPageHeader(t, page, doc)
	checkPageHero(t, page, doc)
	checkPageAgents(t, page, doc)
	checkPageDaily(t, page, doc)
	checkPageWhereItWent(t, page, doc)
	checkPageMostUsed(t, page, doc)
	checkPageHeadsUp(t, page, doc)
	checkPageDetail(t, page, doc)
	checkPageQualifiers(t, page, doc)
}

func checkPageHeader(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	subs := page.byClass("p", "sub")
	if len(subs) == 0 {
		t.Fatal("the page has no subtitle")
	}
	agents := countOf(doc.Coverage.Agents, "agent")
	if got := subs[0].visible(); !strings.Contains(got, agents) || !strings.Contains(got, doc.Window.FirstDay+" to "+doc.Window.LastDay) {
		t.Errorf("the subtitle %q lacks %q and the window's days", got, agents)
	}
	for filter, value := range map[string]string{"harness ": doc.Filters.Harness, "model ": doc.Filters.Model} {
		if value == "" {
			continue
		}
		if got := subs[len(subs)-1].visible(); !strings.Contains(got, filter+value) {
			t.Errorf("the page does not say it is filtered to %s%s: %q", filter, value, got)
		}
	}
}

// expectedDelta is the change the page shows for spend: nothing without a
// previous period.
func expectedDelta(m stats.Measure, days int) string {
	if m.Value == nil || m.Previous == nil || *m.Previous <= 0 || m.ChangePct == nil {
		return ""
	}
	p := math.Round(*m.ChangePct)
	vs := fmt.Sprintf("vs prior %d days", days)
	if days == 1 {
		vs = "vs the day before"
	}
	switch {
	case p > 0:
		return fmt.Sprintf("▲ %s%% %s", statsfmt.CommaInt(int64(p)), vs)
	case p < 0:
		return fmt.Sprintf("▼ %s%% %s", statsfmt.CommaInt(int64(-p)), vs)
	}
	return "no change " + vs
}

func checkPageHero(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	o := doc.Overview
	stat := page.byClass("div", "stat")
	if len(stat) != 3 {
		t.Fatalf("the headline has %d numbers, want spend, sessions and tokens", len(stat))
	}
	part := func(n *node, class string) []string {
		var out []string
		for _, c := range n.children {
			if hasClass(c, class) {
				out = append(out, c.visible())
			}
		}
		return out
	}
	spend := "n/a"
	if o.Tokens.Value != nil {
		spend = jsonCost(doc.Prices.Currency, stats.Cost{USD: o.Cost.Value, Approximate: o.Cost.Approximate, Partial: o.Cost.Partial}, false)
	}
	sessions := "unknown"
	if o.Sessions.Value != nil {
		sessions = statsfmt.CommaInt(int64(math.Round(*o.Sessions.Value)))
	}
	tokens := "unknown"
	if o.Tokens.Value != nil {
		tokens = statsfmt.TokenCount(int64(math.Round(*o.Tokens.Value)))
	}
	var prompts, cache []string
	if o.Prompts.Value != nil {
		prompts = []string{countOf(int(math.Round(*o.Prompts.Value)), "prompt")}
	}
	switch {
	case o.Tokens.Value == nil:
		cache = []string{"no session reports token counts"}
	case o.CacheShare != nil:
		cache = []string{statsfmt.Percent(*o.CacheShare) + " served from cache"}
	}
	for i, want := range []struct {
		label string
		value string
		delta string
		notes []string
	}{
		{"Spend", spend, expectedDelta(o.Cost.Measure, doc.Window.Days), []string{"estimated at list price"}},
		{"Sessions", sessions, "", prompts},
		{"Tokens", tokens, "", cache},
	} {
		n := stat[i]
		if got := part(n, "label"); len(got) != 1 || got[0] != want.label {
			t.Errorf("headline %d: label %q, want %q", i, got, want.label)
		}
		if got := part(n, "value"); len(got) != 1 || got[0] != want.value {
			t.Errorf("%s: value %q, want %q", want.label, got, want.value)
		}
		got := part(n, "delta")
		switch {
		case want.delta == "" && len(got) != 0:
			t.Errorf("%s: shows the change %q with no previous period", want.label, got)
		case want.delta != "" && (len(got) != 1 || got[0] != want.delta):
			t.Errorf("%s: change %q, want %q", want.label, got, want.delta)
		}
		if got := part(n, "note"); !slices.Equal(got, want.notes) {
			t.Errorf("%s: sub-lines %q, want %q", want.label, got, want.notes)
		}
	}
}

func checkPageAgents(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	bar := pageCard(page, "h-agentbar")
	if bar == nil {
		t.Fatal("the page has no agents bar")
	}
	var legend, wantLegend []string
	for _, li := range bar.byClass("ul", "key")[0].children {
		if !hasClass(li, "of") {
			legend = append(legend, li.visible())
		}
	}
	for _, a := range doc.Agents {
		wantLegend = append(wantLegend, a.Label+" "+statsfmt.Percent(a.SessionShare))
	}
	if !slices.Equal(legend, wantLegend) {
		t.Errorf("agents legend %q, want %q", legend, wantLegend)
	}
	table := pageCard(page, "h-agents")
	if table == nil {
		t.Fatal("the page has no agents table")
	}
	var want [][]string
	for _, a := range doc.Agents {
		tokens, cost, cache := "unknown", "n/a", "unknown"
		if a.Tokens != nil {
			tokens, cost, cache = statsfmt.TokenCount(*a.Tokens), jsonCost(doc.Prices.Currency, a.Cost, false), "n/a"
			if a.CacheHitRate != nil {
				cache = statsfmt.Percent(*a.CacheHitRate)
			}
		}
		want = append(want, []string{a.Label, fmt.Sprintf("%s (%s)", statsfmt.CommaInt(int64(a.Sessions)), statsfmt.Percent(a.SessionShare)), tokens, cost, cache})
	}
	if got := rowsOf(table); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("agents table\n got  %q\n want %q", got, want)
	}
}

func checkPageDaily(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	sec := pageCard(page, "h-daily")
	if sec == nil {
		t.Error("the page has no daily spend section")
		return
	}
	if doc.Coverage.SessionsWithTokens == 0 {
		if !strings.Contains(sec.visible(), "No session in this window reports token counts") || len(sec.find(func(m *node) bool { return m.name == "table" })) > 0 {
			t.Errorf("a window without token data draws a chart: %q", sec.visible())
		}
		return
	}
	if doc.PeakSpend == nil {
		if !strings.Contains(sec.visible(), "no spend chart") || len(sec.find(func(m *node) bool { return m.name == "table" })) > 0 {
			t.Errorf("a window with nothing priced draws a chart: %q", sec.visible())
		}
		return
	}
	if len(doc.Daily) > maxDailyBars {
		return
	}
	table := sec.find(func(m *node) bool { return m.name == "table" })[0]
	var want [][]string
	peakCost := stats.Cost{USD: &doc.PeakSpend.USD}
	for _, d := range doc.Daily {
		when, _ := time.Parse("2006-01-02", d.Date)
		label := when.Format("Jan 2")
		if doc.Window.Days > 300 {
			label = when.Format("Jan 2 2006")
		}
		spend := "n/a"
		if d.Sessions == 0 {
			spend = "0"
		} else if _, ok := spendDown(d.Cost); ok {
			spend = jsonCost(doc.Prices.Currency, d.Cost, false)
		}
		tokens := "0"
		switch {
		case d.Tokens == nil && d.Sessions > 0:
			tokens = "unknown"
		case d.Tokens != nil:
			tokens = statsfmt.TokenCount(*d.Tokens)
		}
		want = append(want, []string{label, statsfmt.CommaInt(int64(d.Sessions)), spend, tokens})
		if d.Date == doc.PeakSpend.Date {
			peakCost.Approximate, peakCost.Partial = d.Cost.Approximate, d.Cost.Partial
		}
	}
	if got := rowsOf(table); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("daily table\n got  %q\n want %q", got, want)
	}
	label := sec.byClass("text", "peak-label")
	if len(label) != 1 {
		t.Fatalf("%d peak labels", len(label))
	}
	when, _ := time.Parse("2006-01-02", doc.PeakSpend.Date)
	dayText := when.Format("Jan 2")
	if doc.Window.Days > 300 {
		dayText = when.Format("Jan 2 2006")
	}
	if got, want := label[0].visible(), "Peak "+jsonCost(doc.Prices.Currency, peakCost, false)+" · "+dayText; got != want {
		t.Errorf("peak label %q, want %q", got, want)
	}
	// The dearest day of the table is the peak.
	best := -1.0
	for _, d := range doc.Daily {
		if v, ok := spendDown(d.Cost); ok && v > best {
			best = v
		}
	}
	if best != doc.PeakSpend.USD {
		t.Errorf("peak_spend %v is not the dearest day's %v", doc.PeakSpend.USD, best)
	}
}

func checkPageWhereItWent(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	currency := doc.Prices.Currency
	projects := pageCard(page, "h-projects")
	if projects == nil {
		t.Fatal("the page has no by-project table")
	}
	rows := slices.Clone(doc.Projects)
	slices.SortStableFunc(rows, func(a, b stats.Project) int {
		av, aok := spendDown(a.Cost)
		bv, bok := spendDown(b.Cost)
		switch {
		case aok && bok:
			return cmp.Compare(bv, av)
		case aok:
			return -1
		case bok:
			return 1
		}
		return 0
	})
	var want [][]string
	for _, p := range rows {
		name, cost := p.Name, "n/a"
		if name == "" {
			name = "(no project)"
		}
		if p.Tokens != nil {
			cost = jsonCost(currency, p.Cost, false)
		}
		want = append(want, []string{name, cost, statsfmt.CommaInt(int64(p.Sessions))})
	}
	if got := rowsOf(projects); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("by project\n got  %q\n want %q", got, want)
	}
	if more := doc.TotalProjects - len(doc.Projects); more > 0 && !strings.Contains(projects.visible(), fmt.Sprintf("and %d more projects", more)) {
		t.Errorf("the by-project table does not say %d more projects", more)
	}

	models := pageCard(page, "h-models")
	if len(doc.Models) == 0 {
		if models != nil {
			t.Error("the page has a by-model table without models")
		}
		return
	}
	if models == nil {
		t.Fatal("the page has no by-model table")
	}
	want = nil
	for _, m := range doc.Models[:min(len(doc.Models), 8)] {
		if !m.Priced || m.Cost.USD == nil {
			want = append(want, []string{m.Label, "unpriced", statsfmt.TokenCount(m.Tokens) + " tokens"})
			continue
		}
		share := 0.0
		if m.CostShare != nil {
			share = *m.CostShare
		}
		want = append(want, []string{m.Label, jsonCost(currency, m.Cost, false), statsfmt.Percent(share)})
	}
	if got := rowsOf(models); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("by model\n got  %q\n want %q", got, want)
	}
}

func checkPageMostUsed(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	sec := pageCard(page, "h-most")
	var skills, mcp []string
	for _, s := range doc.DisplaySkills {
		skills = append(skills, s.Name+" "+countOf(s.Sessions, "session"))
	}
	if doc.MCP != nil {
		for _, s := range doc.MCP.Servers {
			calls := statsfmt.CommaInt(s.Calls) + " calls"
			if s.Calls == 1 {
				calls = "1 call"
			}
			mcp = append(mcp, s.Name+" "+calls)
		}
	}
	if len(skills)+len(mcp) == 0 {
		if sec != nil {
			t.Error("the page has a most-used section with nothing in it")
		}
		return
	}
	if sec == nil {
		t.Fatal("the page has no most-used section")
	}
	var gotSkills, gotMCP []string
	for _, row := range sec.byClass("div", "row") {
		var chips []string
		for _, li := range row.byClass("ul", "chips")[0].children {
			chips = append(chips, li.visible())
		}
		switch row.children[0].visible() {
		case "Skills":
			gotSkills = chips
		case "MCP":
			gotMCP = chips
		}
	}
	if !slices.Equal(gotSkills, skills) {
		t.Errorf("skills %q, want %q", gotSkills, skills)
	}
	if !slices.Equal(gotMCP, mcp) {
		t.Errorf("MCP %q, want %q", gotMCP, mcp)
	}
	if doc.MCP != nil && len(mcp) > 0 && !strings.Contains(sec.visible(), "MCP: "+doc.MCP.Scope) {
		t.Error("the MCP scope note is missing")
	}
}

func checkPageHeadsUp(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	sec := pageCard(page, "h-heads")
	if len(doc.HeadsUp) == 0 {
		if sec != nil {
			t.Error("the page has a heads-up section with nothing to say")
		}
		return
	}
	if sec == nil {
		t.Fatal("the page has no heads-up section")
	}
	var got []string
	for _, li := range sec.byClass("ul", "alerts")[0].children {
		got = append(got, li.visible())
	}
	if len(got) != len(doc.HeadsUp) {
		t.Fatalf("%d heads-up lines for %d notes: %q", len(got), len(doc.HeadsUp), got)
	}
	for i, n := range doc.HeadsUp {
		var want []string
		switch n.Kind {
		case stats.NoteSubagentShare:
			want = []string{statsfmt.Percent(*n.Share) + " of tokens came from subagents (" + countOf(*n.Runs, "run") + ")"}
		case stats.NoteCostliestSession:
			want = []string{"Costliest session " + jsonCost(doc.Prices.Currency, *n.Cost, true)}
			if n.Project != "" {
				want = append(want, " · "+n.Project)
			}
		case stats.NoteUnmeteredSessions:
			want = []string{countOf(*n.Sessions, "session")}
			for _, a := range n.ByAgent {
				want = append(want, a.Label+" "+strconv.Itoa(a.Sessions))
			}
		case stats.NoteLowCacheHit:
			want = []string{"Cache hit rate is " + statsfmt.Percent(*n.HitRate) + " over " + statsfmt.TokenCount(*n.InputTokens)}
		default:
			t.Errorf("note %d has a kind the check does not know: %q", i, n.Kind)
		}
		for _, piece := range want {
			if !strings.Contains(got[i], piece) {
				t.Errorf("heads-up %d %q lacks %q", i, got[i], piece)
			}
		}
		if strings.Contains(got[i], "sessions came from") || (n.Kind == stats.NoteSubagentShare && strings.Contains(got[i], "session")) {
			t.Errorf("heads-up %d calls subagent runs sessions: %q", i, got[i])
		}
	}
}

func checkPageDetail(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	tokens := pageCard(page, "h-tokens")
	c := doc.Composition
	if c == nil || c.Total == 0 {
		if tokens != nil {
			t.Error("the page has a composition without token data")
		}
	} else {
		if tokens == nil {
			t.Fatal("the page has no composition")
		}
		var want [][]string
		for _, part := range []struct {
			label string
			seg   stats.Segment
		}{{"Cache read", c.CacheRead}, {"Cache write", c.CacheWrite}, {"Input", c.FreshInput}, {"Output", c.Output}} {
			want = append(want, []string{part.label, statsfmt.Percent(part.seg.Share), statsfmt.TokenCount(part.seg.Tokens)})
		}
		if got := rowsOf(tokens.byClass("table", "legend")[0]); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("composition\n got  %q\n want %q", got, want)
		}
		text := tokens.visible()
		if s := doc.Subagents; s != nil {
			if piece := "Subagents used " + statsfmt.Percent(s.Share) + " of tokens (" + statsfmt.TokenCount(s.Tokens) + ") in " + countOf(s.Sessions, "run"); !strings.Contains(text, piece) {
				t.Errorf("the composition lacks %q", piece)
			}
		} else if strings.Contains(text, "Subagents used") {
			t.Error("the composition talks of subagents that were not used")
		}
	}
	facts := map[string]string{}
	if sec := pageCard(page, "h-facts"); sec != nil {
		for _, div := range sec.byClass("dl", "facts")[0].children {
			facts[div.children[0].visible()] = div.children[1].visible()
		}
	}
	h := doc.Highlights
	check := func(label string, present bool, pieces ...string) {
		text, ok := facts[label]
		if ok != present {
			t.Errorf("fact %q on the page: %v, in the document: %v", label, ok, present)
			return
		}
		for _, piece := range pieces {
			if !strings.Contains(text, piece) {
				t.Errorf("fact %q is %q, lacking %q", label, text, piece)
			}
		}
	}
	active := 0
	if doc.Overview.ActiveDays.Value != nil {
		active = int(math.Round(*doc.Overview.ActiveDays.Value))
	}
	check("Days active", doc.Overview.ActiveDays.Value != nil, fmt.Sprintf("%d of %d days", active, doc.Overview.DaysInWindow))
	if h.BusiestDay != nil {
		check("Busiest day", true, countOf(h.BusiestDay.Sessions, "session"))
	} else {
		check("Busiest day", false)
	}
	check("Favorite model", h.FavoriteModel != nil)
	check("Costliest session", h.CostliestSession != nil && h.CostliestSession.Cost.USD != nil)
	if te := h.ToolErrors; te != nil {
		check("Tool errors", true, statsfmt.RatePercent(te.Rate)+" of "+statsfmt.CommaInt(te.Results)+" tool results flagged as errors", countOf(te.Sessions, "session")+" measured")
	} else {
		check("Tool errors", false)
	}
	if m := h.MonthRank; m != nil && m.Of > 1 {
		check("Month rank", true)
	} else {
		check("Month rank", false)
	}
	if g := doc.Groups; g != nil && len(g.Rows) > 0 && pageCard(page, "h-groups") == nil {
		t.Error("the page lacks the breakdown --by asked for")
	}
	if doc.Groups == nil && pageCard(page, "h-groups") != nil {
		t.Error("the page has a breakdown nobody asked for")
	}
}

// checkPageQualifiers checks the footer's explanation of ~ and +: it is there
// exactly when a cost shown carries the qualifier (for a window of more days
// than the chart has bars, when some cost of the document does).
func checkPageQualifiers(t *testing.T, page *node, doc statsDocument) {
	t.Helper()
	var approx, partial bool
	note := func(c stats.Cost) {
		if c.USD == nil {
			return
		}
		approx = approx || c.Approximate
		partial = partial || c.Partial
	}
	if doc.Overview.Tokens.Value != nil {
		note(stats.Cost{USD: doc.Overview.Cost.Value, Approximate: doc.Overview.Cost.Approximate, Partial: doc.Overview.Cost.Partial})
	}
	for _, a := range doc.Agents {
		if a.Tokens != nil {
			note(a.Cost)
		}
	}
	for _, m := range doc.Models[:min(len(doc.Models), 8)] {
		if m.Priced {
			note(m.Cost)
		}
	}
	for _, p := range doc.Projects {
		if p.Tokens != nil {
			note(p.Cost)
		}
	}
	if doc.Groups != nil {
		for _, r := range doc.Groups.Rows {
			if r.Tokens != nil {
				note(r.Cost)
			}
		}
	}
	for _, d := range doc.Daily {
		note(d.Cost)
	}
	for _, n := range doc.HeadsUp {
		if n.Cost != nil {
			note(*n.Cost)
		}
	}
	if c := doc.Highlights.CostliestSession; c != nil {
		note(c.Cost)
	}
	foot := page.find(func(m *node) bool { return m.name == "footer" })[0].visible()
	hasApprox := strings.Contains(foot, "~ priced at the session's main model")
	hasPartial := strings.Contains(foot, "+ leaves out tokens of models the price table does not list")
	exact := len(doc.Daily) <= maxDailyBars
	for _, q := range []struct {
		name   string
		onPage bool
		inDoc  bool
	}{{"~", hasApprox, approx}, {"+", hasPartial, partial}} {
		if q.onPage && !q.inDoc || exact && q.inDoc && !q.onPage {
			t.Errorf("the footer explains %s: %v, the costs shown carry it: %v", q.name, q.onPage, q.inDoc)
		}
	}
	for _, want := range []string{
		"Scope: this archive only.", "Cost is an estimate at list price, not a bill.",
		fmt.Sprintf("Token data: %d of %s.", doc.Coverage.SessionsWithTokens, countOf(doc.Coverage.Sessions, "session")),
		"Prices " + doc.Prices.Version + ", as of " + doc.Prices.AsOf,
	} {
		if !strings.Contains(foot, want) {
			t.Errorf("the footer lacks %q", want)
		}
	}
	if doc.Coverage.SubagentSessions > 0 && !strings.Contains(foot, countOf(doc.Coverage.SubagentSessions, "subagent run")) {
		t.Error("the footer does not count the subagent runs")
	}
}
