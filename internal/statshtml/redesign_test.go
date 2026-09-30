package statshtml

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// futureNoteKind is a note kind a newer engine might send.
const futureNoteKind stats.NoteKind = "future_kind"

func realisticPage(t *testing.T, opts Options) string {
	t.Helper()
	return string(render(t, modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone), opts))
}

// The colors clear WCAG in both themes: text at 4.5:1 and marks at 3:1 against
// the surface, and the black label on a ring segment at 4.5:1.
func TestPaletteContrast(t *testing.T) {
	t.Parallel()
	light, dark := themeColors(t)
	for name, theme := range map[string]map[string]string{"light": light, "dark": dark} {
		surface := theme["surface"]
		for _, text := range []string{"ink", "ink-2", "ink-3", "amber-text", "green-text"} {
			if got := contrast(t, theme[text], surface); got < 4.5 {
				t.Errorf("%s: --%s on the surface is %.2f:1, under 4.5:1", name, text, got)
			}
			if got := contrast(t, theme[text], theme["page"]); got < 4.5 {
				t.Errorf("%s: --%s on the page is %.2f:1, under 4.5:1", name, text, got)
			}
		}
		for _, mark := range []string{
			"cyan", "cyan-strong", "claude", "cursor", "codex", "opus", "fable", "sonnet", "haiku", "gpt", "other",
			"seg1", "seg2", "seg3", "seg4", "amber", "unknown",
		} {
			if got := contrast(t, theme[mark], surface); got < 3 {
				t.Errorf("%s: --%s against the surface is %.2f:1, under 3:1", name, mark, got)
			}
		}
		// Labels on the ring are black; each ring color must carry them.
		for _, seg := range []string{"seg1", "seg2", "seg3", "seg4"} {
			if got := contrast(t, "#000000", theme[seg]); got < 4.5 {
				t.Errorf("%s: black label on --%s is %.2f:1, under 4.5:1", name, seg, got)
			}
		}
	}
}

// One color means one thing: the three agents, the model families and the ring
// segments are each told apart within a theme (the OpenAI models share the
// green the Codex agent has, as in the terminal), and the terminal's pairings
// hold: Claude Code orange, Cursor blue, Codex green.
func TestSemanticColorsAreDistinct(t *testing.T) {
	t.Parallel()
	light, dark := themeColors(t)
	for name, theme := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, group := range [][]string{
			{"claude", "cursor", "codex"},
			{"opus", "fable", "sonnet", "haiku", "gpt", "cyan"},
			{"seg1", "seg2", "seg3", "seg4"},
		} {
			seen := map[string]string{}
			for _, token := range group {
				if other, dup := seen[theme[token]]; dup {
					t.Errorf("%s: --%s and --%s are both %s", name, token, other, theme[token])
				}
				seen[theme[token]] = token
			}
		}
		if theme["gpt"] != theme["codex"] {
			t.Errorf("%s: the OpenAI models are not the Codex green", name)
		}
	}
}

// The change against the previous period is shown for spend, with its words
// for a screen reader, when there is a previous period; without one the page
// says nothing (never "new").
func TestSpendDeltaOnlyWithAPreviousPeriod(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	if !strings.Contains(out, `<div class="delta up"><span aria-hidden="true">▲ 486%</span><span class="sr">up 486 percent</span> <span class="vs">vs prior 30 days</span></div>`) {
		t.Error("the spend delta lacks its arrow, spoken form or comparison")
	}
	if got := strings.Count(out, `class="delta `); got != 1 {
		t.Errorf("%d deltas, want spend's alone", got)
	}
	none := realisticPage(t, Options{})
	if strings.Contains(none, `class="delta `) || strings.Contains(none, ">new<") || strings.Contains(none, "vs prior") {
		t.Error("a window with no previous period shows a change")
	}
}

func TestDeltaTextStates(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		m       stats.Measure
		glyph   string
		spoken  string
		dir     string
		nothing bool
	}{
		{"up", stats.Measure{Value: f(118), Previous: f(100), ChangePct: f(18)}, "▲ 18%", "up 18 percent", "up", false},
		{"down", stats.Measure{Value: f(82), Previous: f(100), ChangePct: f(-18)}, "▼ 18%", "down 18 percent", "down", false},
		{"flat", stats.Measure{Value: f(100), Previous: f(100), ChangePct: f(0.2)}, "no change", "no change", "flat", false},
		// Against next to nothing the exact figure only measures how little there was.
		{"999 percent", stats.Measure{Value: f(1099), Previous: f(100), ChangePct: f(999)}, "▲ 999%", "up 999 percent", "up", false},
		{"past 999 percent", stats.Measure{Value: f(3), Previous: f(0.0001), ChangePct: f(2_999_900)}, "▲ >999%", "up more than 999 percent", "up", false},
		{"beyond any integer", stats.Measure{Value: f(1e30), Previous: f(1), ChangePct: f(1e32)}, "▲ >999%", "up more than 999 percent", "up", false},
		{"nothing before", stats.Measure{Value: f(5), Previous: f(0)}, "", "", "", true},
		{"unknown before", stats.Measure{Value: f(5)}, "", "", "", true},
		{"unknown now", stats.Measure{Previous: f(5)}, "", "", "", true},
		{"no percentage", stats.Measure{Value: f(5), Previous: f(5)}, "", "", "", true},
		{"not a number", stats.Measure{Value: f(5), Previous: f(5), ChangePct: f(math.NaN())}, "", "", "", true},
		{"negative previous", stats.Measure{Value: f(5), Previous: f(-5), ChangePct: f(200)}, "", "", "", true},
	}
	for _, tc := range cases {
		glyph, spoken, dir := deltaText(tc.m)
		if glyph != tc.glyph || spoken != tc.spoken || dir != tc.dir {
			t.Errorf("%s: deltaText = %q, %q, %q; want %q, %q, %q", tc.name, glyph, spoken, dir, tc.glyph, tc.spoken, tc.dir)
		}
	}
}

// The headline trio is spend, sessions and tokens, each with its sub-lines:
// prompts under sessions, the cache share under tokens, and no shaded track,
// which was noise, behind any bar.
func TestHeadlineTrio(t *testing.T) {
	t.Parallel()
	out := realisticPage(t, Options{})
	hero := out[strings.Index(out, `id="h-headline"`):strings.Index(out, `id="h-agentbar"`)]
	for _, want := range []string{
		`<div class="label">Spend</div>`, `<div class="label">Sessions</div>`, `<div class="label">Tokens</div>`,
		`<div class="value">93</div>`, `<div class="note">872 prompts</div>`, `<div class="note">94% served from cache</div>`,
		`<div class="note">estimated at list price</div>`, `<div class="value">10B</div>`,
	} {
		if !strings.Contains(hero, want) {
			t.Errorf("the headline lacks %q", want)
		}
	}
	if strings.Count(hero, `class="stat"`) != 3 {
		t.Error("the headline is not three numbers")
	}
	// Days active, prompts and the month rank are detail, not headline.
	for _, detail := range []string{"Days active", "Month rank", "Busiest day"} {
		if strings.Contains(hero, detail) {
			t.Errorf("the headline holds %q", detail)
		}
	}
	if strings.Contains(out, `class="track"`) || strings.Contains(styleSheet, "--track") {
		t.Error("a bar still has a shaded track")
	}
}

// A Cursor-only window reads unknown tokens and n/a spend in the trio, and
// says why the token line is what it is.
func TestHeadlineForUnknownTokens(t *testing.T) {
	t.Parallel()
	cursor := sessionSpec{id: "c1", harness: "cursor", project: "p", captured: day(time.September, 20, 10), models: []string{"cursor-auto"}, turns: 3}
	out := string(render(t, computeFixture(t, []archive.Metadata{cursor.build()}, 30, stats.GroupNone), Options{}))
	for _, want := range []string{`<div class="value">n/a</div>`, `<div class="value">unknown</div>`, "no session reports token counts"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Cursor-only headline lacks %q", want)
		}
	}
}

// The agents are one stacked bar in the agents' colors, each segment as wide as
// the agent's share of sessions, and a legend that names each with its share, so
// the colors are never the only way to tell them apart.
func TestAgentBar(t *testing.T) {
	t.Parallel()
	out := realisticPage(t, Options{})
	section := out[strings.Index(out, `id="h-agentbar"`):strings.Index(out, `id="h-daily"`)]
	for _, want := range []string{
		`class="fill-agent-claude"`, `class="fill-agent-cursor"`, `class="fill-agent-codex"`,
		`aria-label="Sessions by agent: Claude Code 90%, Cursor 9%, Codex 1%."`,
		`Claude Code <span class="pct">90%</span>`, `Cursor <span class="pct">9%</span>`, `Codex <span class="pct">1%</span>`,
		`of sessions`, `<title>Claude Code · 84 sessions · 90%</title>`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the agents bar lacks %q", want)
		}
	}
	// The segments tile the bar from 0 to at most 100%, with no overlap.
	seg := regexp.MustCompile(`<rect class="fill-agent-[a-z]+" x="([0-9.]+)%" width="([0-9.]+)%"`).FindAllStringSubmatch(section, -1)
	if len(seg) != 3 {
		t.Fatalf("%d segments, want 3", len(seg))
	}
	end := 0.0
	for _, m := range seg {
		x, _ := strconv.ParseFloat(m[1], 64)
		w, _ := strconv.ParseFloat(m[2], 64)
		if math.Abs(x-end) > 0.02 {
			t.Errorf("a segment starts at %.2f%% where the last ended at %.2f%%", x, end)
		}
		end = x + w
	}
	if end > 100.05 || end < 99.9 {
		t.Errorf("the segments end at %.2f%%, want 100%%", end)
	}
}

// Shares that are wrong (over 100%, negative, not a number) cannot make the
// stacked bar overflow its width.
func TestAgentBarNeverOverflows(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupNone)
	s = deepCopy(s)
	for i, share := range []float64{0.9, 5, math.NaN()} {
		if i < len(s.Agents) {
			s.Agents[i].SessionShare = share
		}
	}
	out, err := Render(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	end := 0.0
	for _, m := range regexp.MustCompile(`<rect class="fill-agent-[a-z]+" x="([0-9.]+)%" width="([0-9.]+)%"`).FindAllStringSubmatch(string(out), -1) {
		x, _ := strconv.ParseFloat(m[1], 64)
		w, _ := strconv.ParseFloat(m[2], 64)
		end = math.Max(end, x+w)
	}
	if end > 100.01 {
		t.Errorf("the segments reach %.2f%%", end)
	}
}

// The agents' colors follow the harness, and an agent the page has no color for
// is neutral.
func TestHarnessClasses(t *testing.T) {
	t.Parallel()
	for harness, want := range map[string]string{"claude": "claude", "cursor": "cursor", "codex": "codex", "": "other", "gemini": "other", `"><script>`: "other"} {
		if got := harnessClass(harness); got != want {
			t.Errorf("harnessClass(%q) = %q, want %q", harness, got, want)
		}
	}
}

// Where it went lists projects dearest first with a bar against the dearest, and
// the models in their family colors; the stand-ins of a model the page does not
// name are neutral, so a color tells nothing about a hidden model.
func TestWhereItWent(t *testing.T) {
	t.Parallel()
	s := modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone)
	out := string(render(t, s, Options{IncludeNames: true}))
	// Projects come dearest first, as the engine ranks them.
	var want []string
	for i, r := range s.Projects {
		if i > 0 && *r.Cost.USD > *s.Projects[i-1].Cost.USD {
			t.Fatalf("the engine's projects are not dearest first: %v", s.Projects)
		}
		want = append(want, r.Name)
	}
	table := out[strings.Index(out, `id="h-projects"`):strings.Index(out, `id="h-models"`)]
	last := -1
	for _, name := range want {
		at := strings.Index(table, `<th scope="row">`+name+`</th>`)
		if at < 0 {
			t.Fatalf("the projects table lacks %q", name)
		}
		if at < last {
			t.Errorf("%q is out of spend order", name)
		}
		last = at
	}
	if !strings.Contains(table, `class="fill-project" width="100%"`) {
		t.Error("the dearest project's bar is not the full width")
	}
	for _, class := range []string{"opus", "fable", "sonnet", "haiku", "gpt"} {
		if !strings.Contains(out, `class="fill-`+class+`"`) {
			t.Errorf("no model bar in the %s color", class)
		}
	}
	// A hidden model is neutral even when its name would have a color.
	hidden := []archive.Metadata{sessionSpec{
		id: "h1", harness: "codex", project: "p", captured: day(time.September, 20, 10),
		models: []string{"gpt-internal-zqcorp"}, turns: 3,
		tokens: []tokenSpec{{"gpt-internal-zqcorp", 10_000, 1_000, 5_000, 0}},
	}.build()}
	hs := modelStats(t, hidden, "", stats.GroupNone)
	shown := string(render(t, hs, Options{IncludeNames: true}))
	secret := string(render(t, hs, Options{}))
	if !strings.Contains(shown, `class="fill-gpt"`) {
		t.Error("a named gpt model has no gpt color")
	}
	if strings.Contains(secret, `class="fill-gpt"`) || !strings.Contains(secret, `class="fill-other"`) {
		t.Error("a hidden model is colored by the family its name suggests")
	}
	if strings.Contains(secret, "zqcorp") {
		t.Error("the hidden model's name is on the page")
	}
}

// The by-project list is the engine's top few projects by spend. The note says
// how many more there are and nothing about what they cost or how many tokens
// they have: the page shows no figures for rows it leaves out.
func TestByProjectNoteSaysOnlyHowManyMore(t *testing.T) {
	t.Parallel()
	s := modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone)
	more := s.TotalProjects - len(s.Projects)
	if more <= 0 {
		t.Fatalf("the fixture has %d projects for %d shown; it must have more", s.TotalProjects, len(s.Projects))
	}
	out := string(render(t, s, Options{}))
	table := out[strings.Index(out, `id="h-projects"`):strings.Index(out, `id="h-models"`)]
	if want := "and " + strconv.Itoa(more) + " more projects, not shown"; !strings.Contains(table, want) {
		t.Errorf("the projects table does not say %q", want)
	}
	for _, claim := range []string{"fewer", "less", "cheaper", "dearer", "more tokens", "most tokens", "necessarily", "top "} {
		if strings.Contains(strings.ToLower(table), claim) {
			t.Errorf("the projects table makes a claim about the rest (%q)", claim)
		}
	}
}

// The projects the page keeps are the top few by spend, not by tokens: when
// cache reads give five projects more tokens than two output-heavy ones, the
// two that cost more are still on the page, above the cheaper ones, and the
// projects left out are three that cost less.
func TestByProjectListKeepsTheDearestProjects(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	for i := range 5 {
		sessions = append(sessions, sessionSpec{
			id: fmt.Sprintf("cache-%d", i), harness: "claude", project: fmt.Sprintf("cache-heavy-%d", i), captured: day(time.September, 20, 9),
			models: []string{"claude-opus-5"}, turns: 3,
			tokens: []tokenSpec{{"claude-opus-5", 0, 0, 10_000_000 + i*1_000_000, 0}},
		}.build())
	}
	for i := range 2 {
		sessions = append(sessions, sessionSpec{
			id: fmt.Sprintf("out-%d", i), harness: "claude", project: fmt.Sprintf("output-heavy-%d", i), captured: day(time.September, 21, 9),
			models: []string{"claude-opus-5"}, turns: 3,
			tokens: []tokenSpec{{"claude-opus-5", 0, 1_000_000 + i*100_000, 0, 0}},
		}.build())
	}
	s := modelStats(t, sessions, realisticPrices, stats.GroupNone)
	if s.TotalProjects != 7 || len(s.Projects) != 5 {
		t.Fatalf("%d projects, %d kept; want 7 and 5", s.TotalProjects, len(s.Projects))
	}
	out := string(render(t, s, Options{IncludeNames: true}))
	table := out[strings.Index(out, `id="h-projects"`):strings.Index(out, `id="h-models"`)]
	var got []string
	for _, row := range strings.Split(table, `<th scope="row">`)[1:] {
		name, _, _ := strings.Cut(row, "</th>")
		got = append(got, name)
	}
	want := []string{"output-heavy-1", "output-heavy-0", "cache-heavy-4", "cache-heavy-3", "cache-heavy-2"}
	if !slices.Equal(got, want) {
		t.Errorf("the projects table lists %v, want %v", got, want)
	}
	if !strings.Contains(table, "and 2 more projects, not shown") {
		t.Error("the projects table does not say how many more there are")
	}
}

// Most used lists skills by the sessions that used them, with a plugin prefix
// stripped for display (and the merged spellings counted once), and MCP servers
// by calls with the scope note. Names are stand-ins unless asked for.
func TestMostUsed(t *testing.T) {
	t.Parallel()
	named := realisticPage(t, Options{IncludeNames: true})
	section := named[strings.Index(named, `id="h-most"`):strings.Index(named, `id="h-heads"`)]
	for _, want := range []string{
		`<span class="chip-name">code-review</span> <span class="chip-count">`, `<span class="chip-name">docs</span>`,
		`<span class="chip-name">github</span> <span class="chip-count">81 calls</span>`,
		"MCP: Claude Code and Cursor only; Codex MCP calls are not recorded.",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("most used lacks %q", want)
		}
	}
	if strings.Contains(named, "anthropic-skills:") {
		t.Error("a plugin prefix is shown")
	}
	hidden := realisticPage(t, Options{})
	for _, secret := range []string{"code-review", "review-pr", "github", "linear", "cursor-guide"} {
		if strings.Contains(hidden, secret) {
			t.Errorf("the shareable page names %q", secret)
		}
	}
	for _, want := range []string{"skill A", "MCP server A", "Skill and MCP server names are replaced by letters"} {
		if !strings.Contains(hidden, want) {
			t.Errorf("the shareable page lacks %q", want)
		}
	}
	// Nothing used, nothing shown.
	bare := string(render(t, computeFixture(t, []archive.Metadata{
		sessionSpec{id: "n", harness: "claude", project: "p", captured: day(time.September, 20, 10), models: []string{"claude-opus-5"}, turns: 2,
			tokens: []tokenSpec{{"claude-opus-5", 1, 1, 1, 1}}}.build()}, 30, stats.GroupNone), Options{}))
	if strings.Contains(bare, "Most used") {
		t.Error("the page has a most-used section with nothing in it")
	}
}

// Heads up is the engine's notes as sentences: runs are runs, never sessions;
// the project of the costliest session goes through the same stand-ins as every
// other project; and a note missing a number is left out.
func TestHeadsUpSentences(t *testing.T) {
	t.Parallel()
	out := realisticPage(t, Options{})
	section := out[strings.Index(out, `id="h-heads"`):strings.Index(out, `id="h-detail"`)]
	for _, want := range []string{
		"<li>82% of tokens came from subagents (497 runs)</li>",
		"<li>10 sessions have no token data (Cursor 8, Claude Code 2)</li>",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("heads up lacks %q", want)
		}
	}
	if !regexp.MustCompile(`<li>Costliest session \$[0-9,.]+ · project [A-Z] · long context, 38 subagents</li>`).MatchString(section) {
		t.Errorf("heads up lacks the costliest session's sentence:\n%s", section)
	}
	if strings.Contains(section, "subagent sessions") || strings.Contains(section, "styleprofile") {
		t.Error("heads up calls runs sessions or names the project")
	}
	named := realisticPage(t, Options{IncludeNames: true})
	if !strings.Contains(named, " · styleprofile · long context, 38 subagents</li>") {
		t.Error("the page with real names does not name the costliest session's project")
	}
	// The order and number of notes are the engine's.
	if got := strings.Count(section, "<li>"); got != 3 {
		t.Errorf("%d notes, want the three of the fixture", got)
	}

	s := deepCopy(modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone))
	// A note without its numbers, and a kind the page does not know, say nothing.
	s.HeadsUp = []stats.Note{{Kind: stats.NoteSubagentShare}, {Kind: stats.NoteCostliestSession}, {Kind: stats.NoteUnmeteredSessions}, {Kind: stats.NoteLowCacheHit}, {Kind: futureNoteKind}}
	page := string(render(t, s, Options{}))
	if strings.Contains(page, "Heads up") {
		t.Error("notes without their numbers are still shown")
	}
	one := 1
	rate, tokens := 0.4, int64(120_000_000)
	s.HeadsUp = []stats.Note{
		{Kind: stats.NoteUnmeteredSessions, Sessions: &one, ByAgent: []stats.AgentSessions{{Harness: "cursor", Label: "Cursor", Sessions: 1}}},
		{Kind: stats.NoteLowCacheHit, HitRate: &rate, InputTokens: &tokens},
	}
	page = string(render(t, s, Options{}))
	for _, want := range []string{"<li>1 session has no token data (Cursor 1)</li>", "<li>Cache hit rate is 40% over 120M input-side tokens, which is low</li>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// The detail follows the default view under its own divider: the agents table
// (with the cache-hit rate), the token composition, and the facts, including
// days active and the tool-error rate with its sample size.
func TestDetailFollowsTheDefaultView(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	divider := strings.Index(out, `id="h-detail"`)
	if divider < 0 {
		t.Fatal("no divider before the detail")
	}
	for _, before := range []string{`id="h-headline"`, `id="h-agentbar"`, `id="h-daily"`, `id="h-projects"`, `id="h-most"`, `id="h-heads"`} {
		if at := strings.Index(out, before); at < 0 || at > divider {
			t.Errorf("%s is not above the detail", before)
		}
	}
	for _, after := range []string{`id="h-agents"`, `id="h-tokens"`, `id="h-facts"`} {
		if at := strings.Index(out, after); at < divider {
			t.Errorf("%s is not below the divider", after)
		}
	}
	for _, want := range []string{
		"<th scope=\"col\" class=\"num\">Cache hit</th>", "<dt>Days active</dt>", "<dt>Busiest day</dt>", "<dt>Favorite model</dt>",
		"<dt>Month rank</dt>", "<dt>Tool errors</dt>", "flagged as errors", "measured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the detail lacks %q", want)
		}
	}
	if strings.Contains(out, "subagent sessions") {
		t.Error("the page calls subagent runs sessions")
	}
	if !strings.Contains(out, "subagent runs counted with their parent sessions") {
		t.Error("the footer does not say runs")
	}
}

// The print sheet gives the stacked bar patterns, so the agents are told apart
// in black and white, and the patterns exist on a page with no donut.
func TestPrintPatternsExistWithoutADonut(t *testing.T) {
	t.Parallel()
	cursor := sessionSpec{id: "c1", harness: "cursor", project: "p", captured: day(time.September, 20, 10), models: []string{"cursor-auto"}, turns: 3}
	out := string(render(t, computeFixture(t, []archive.Metadata{cursor.build()}, 30, stats.GroupNone), Options{}))
	for _, id := range []string{"hatch1", "hatch2", "hatch3", "hatch4"} {
		if !strings.Contains(out, `id="`+id+`"`) {
			t.Errorf("the page has no %s pattern", id)
		}
	}
	for _, rule := range []string{".fill-agent-claude { fill: url(#hatch1); }", ".fill-agent-cursor { fill: url(#hatch2); }", ".fill-agent-codex { fill: url(#hatch3); }"} {
		if !strings.Contains(styleSheet, rule) {
			t.Errorf("the stylesheet lacks %q", rule)
		}
	}
}

// On paper the peak day's bar is darker than a priced day's and an unknown
// day's mark is lighter, so a black-and-white print still shows which day was
// dearest. The rules that win for each bar (by specificity, then order) among
// the screen rules and the print sheet must give three different fills; the
// peak once lost to a more specific rule of the screen sheet.
func TestPrintedDailyBarsAreToldApart(t *testing.T) {
	t.Parallel()
	forcedAt := strings.Index(styleSheet, "@media (forced-colors")
	if !strings.Contains(styleSheet, "@media print") || forcedAt < 0 {
		t.Fatal("the stylesheet lacks its print or forced-colors sheet")
	}
	rule := regexp.MustCompile(`(svg\.daily \.[.a-z]+)\s*\{\s*fill:\s*([^;]+);`)
	winner := func(classes ...string) string {
		bestSpecificity, fill := -1, ""
		for _, m := range rule.FindAllStringSubmatch(styleSheet[:forcedAt], -1) {
			needed := strings.Split(strings.TrimPrefix(strings.TrimPrefix(m[1], "svg.daily "), "."), ".")
			if slices.ContainsFunc(needed, func(c string) bool { return !slices.Contains(classes, c) }) {
				continue
			}
			// The svg element and its class, then the bar's classes.
			if specificity := 2 + len(needed); specificity >= bestSpecificity {
				bestSpecificity, fill = specificity, m[2]
			}
		}
		if bestSpecificity < 0 {
			t.Fatalf("no rule colors a bar of %v", classes)
		}
		return fill
	}
	peak, day, unknown := winner("bar", "day", "peak"), winner("bar", "day"), winner("bar", "unknown")
	for _, fill := range []string{peak, day, unknown} {
		if !strings.HasPrefix(fill, "#") {
			t.Errorf("a printed bar is colored %q, not by the print sheet", fill)
		}
	}
	if peak == day || unknown == day || peak == unknown {
		t.Errorf("printed bars are not told apart: peak %s, day %s, unknown %s", peak, day, unknown)
	}
}

// A day whose cost is not a number a chart can draw is an unknown day, not a
// bar: the guard behind the daily chart's geometry.
func TestSpendOfRefusesWhatCannotBeDrawn(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		usd *float64
		ok  bool
	}{{nil, false}, {f(math.NaN()), false}, {f(math.Inf(1)), false}, {f(-1), false}, {f(0), true}, {f(2.5), true}} {
		if _, ok := spendOf(stats.Cost{USD: tc.usd}); ok != tc.ok {
			t.Errorf("spendOf(%v) ok = %v, want %v", tc.usd, ok, tc.ok)
		}
	}
}

// A window whose sessions could not be priced says so instead of drawing an
// empty chart, and a window whose tokens are known but not priced does too.
func TestNoSpendChartWithoutPricedDays(t *testing.T) {
	t.Parallel()
	unpriced := sessionSpec{
		id: "u1", harness: "codex", project: "p", captured: day(time.September, 20, 10), models: []string{"mystery-1"}, turns: 3,
		tokens: []tokenSpec{{"mystery-1", 50_000, 5_000, 20_000, 0}},
	}
	out := string(render(t, computeFixture(t, []archive.Metadata{unpriced.build()}, 30, stats.GroupNone), Options{}))
	if !strings.Contains(out, "no spend chart") || strings.Contains(out, `class="daily"`) {
		t.Error("a window with nothing priced draws a spend chart or does not say why")
	}
}

// The number of days a bar covers is said where the table is headed.
func TestLongWindowsHeadTheTableWithBusiestDay(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	for i := range 200 {
		sessions = append(sessions, sessionSpec{
			id: "s" + strconv.Itoa(i), harness: "claude", project: "p", captured: fixtureNow.AddDate(0, 0, -i*3),
			models: []string{"claude-opus-5"}, turns: 3, tokens: []tokenSpec{{"claude-opus-5", 10_000, 5_000, 100_000, 10_000}},
		}.build())
	}
	out := string(render(t, computeFixture(t, sessions, 365, stats.GroupNone), Options{}))
	for _, want := range []string{"Busiest day&#39;s spend", "Busiest day&#39;s tokens", "Each bar is 4 days and shows its busiest day."} {
		if !strings.Contains(out, want) {
			t.Errorf("the long window lacks %q", want)
		}
	}
}
