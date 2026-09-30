package statshtml

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

func render(t *testing.T, s stats.Stats, opts Options) []byte {
	t.Helper()
	if opts.GeneratedAt.IsZero() {
		opts.GeneratedAt = fixtureNow
	}
	out, err := Render(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	checkPage(t, out)
	return out
}

func goldenPath(name string) string { return filepath.Join("testdata", name+".html.golden") }

// The page is pinned for a multi-agent archive with several models,
// subagents, Cursor sessions without tokens and an unpriced model: by default
// (project names replaced) with a weekly breakdown, and with real names and a
// breakdown by project. A change to the page shows up as a golden diff to
// review.
func TestPageGoldens(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	golden.Check(t, goldenPath("shareable"), render(t, computeFixture(t, sessions, 30, stats.GroupWeek), Options{}))
	golden.Check(t, goldenPath("real-names"), render(t, computeFixture(t, sessions, 30, stats.GroupProject),
		Options{IncludeProjectNames: true, Filters: Filters{Harness: "claude", Origin: "hook"}}))
	golden.Check(t, goldenPath("empty"), render(t, computeFixture(t, nil, 30, stats.GroupNone), Options{}))
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	first := render(t, computeFixture(t, sessions, 30, stats.GroupDay), Options{})
	for range 3 {
		if again := render(t, computeFixture(t, sessions, 30, stats.GroupDay), Options{}); !bytes.Equal(first, again) {
			t.Fatal("two renders of the same stats differ")
		}
	}
}

// The default page carries no project name, and the page as a whole carries
// no session ID, path or prompt-shaped text: aggregates only.
func TestShareablePageHasNoProjectNamesOrIDs(t *testing.T) {
	t.Parallel()
	sessions := fixtureSessions()
	out := string(render(t, computeFixture(t, sessions, 30, stats.GroupProject), Options{}))
	for _, secret := range []string{"agent-archive", "proj-api", "dotfiles"} {
		// The page names the tool itself; the project must not be named.
		text := strings.ReplaceAll(out, "agent-archive stats", "")
		text = strings.ReplaceAll(text, "agent-archive --", "")
		if strings.Contains(text, secret) {
			t.Errorf("the shareable page names the project %q", secret)
		}
	}
	for _, id := range []string{"claude-big", "claude-00", "codex-00", "cursor-00", "sessions/", "source.jsonl"} {
		if strings.Contains(out, id) {
			t.Errorf("the page contains %q, which looks like a session ID or path", id)
		}
	}
	if !strings.Contains(out, "project A") || !strings.Contains(out, "project B") {
		t.Error("the page has no stand-in labels")
	}
	if !strings.Contains(out, "--include-project-names") {
		t.Error("the page does not say how to show project names")
	}
}

// One project has one label throughout the file: its row in top projects, the
// costliest session, and the breakdown by project agree.
func TestProjectLabelsAreConsistentAcrossSections(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupProject)
	out := string(render(t, s, Options{}))
	names := newNamer(false, "project")
	first := names.project(s.Projects[0].Name)
	if first != "project A" {
		t.Fatalf("the top project is %q, want project A", first)
	}
	costliest := s.Highlights.CostliestSession.Project
	label := names.project(costliest)
	if !strings.Contains(out, label+" (long context") && !strings.Contains(out, "· "+label) {
		t.Errorf("the costliest session's project %q is not labelled %q in the highlights", costliest, label)
	}
	// Every project of the breakdown has a label, and labels are not reused.
	seen := map[string]string{}
	for _, row := range s.Groups.Rows {
		l := names.project(row.Key)
		if other, dup := seen[l]; dup && other != row.Key {
			t.Errorf("label %q names both %q and %q", l, other, row.Key)
		}
		seen[l] = row.Key
	}
	if strings.Count(out, "project A") < 3 {
		t.Errorf("project A appears %d times, want it in top projects, highlights and the breakdown", strings.Count(out, "project A"))
	}
}

func TestLettersCountLikeSpreadsheetColumns(t *testing.T) {
	t.Parallel()
	for i, want := range map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		if got := letters(i); got != want {
			t.Errorf("letters(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestIncludeProjectNamesShowsThem(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupProject), Options{IncludeProjectNames: true}))
	for _, name := range []string{"agent-archive", "proj-api", "dotfiles"} {
		if !strings.Contains(out, name) {
			t.Errorf("the page with real names does not show %q", name)
		}
	}
	if strings.Contains(out, "project A") || strings.Contains(out, "replaced by letters") {
		t.Error("the page with real names still talks about stand-ins")
	}
}

// Every name that reaches the page from an archive is escaped or cleaned: a
// project, model, skill or MCP server name built to break out of its element
// or its attribute, to run script, to hide text with a right-to-left override,
// or to end a line, comes out as inert text.
func TestHostileNamesAreInertText(t *testing.T) {
	t.Parallel()
	for i, payload := range hostilePayloads {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			s := computeFixture(t, hostileSessions(payload), 30, stats.GroupProject)
			for _, reveal := range []bool{true, false} {
				out := render(t, s, Options{
					IncludeProjectNames: reveal,
					Filters:             Filters{Harness: payload, Model: payload, Origin: payload},
				})
				assertInert(t, string(out), payload)
			}
		})
	}
}

// hostileSessions are sessions whose project, model, skill and MCP names are
// the payload.
func hostileSessions(payload string) []archive.Metadata {
	spec := sessionSpec{
		id: "hostile", harness: "claude", project: payload, captured: day(time.September, 20, 10),
		models: []string{payload}, turns: 5, messages: 20, toolResults: 10, errors: 1,
		tokens: []tokenSpec{{payload, 1_000, 2_000, 30_000, 4_000}},
		skills: []string{payload}, mcp: map[string]int{payload: 3},
	}
	other := spec
	other.id, other.project = "hostile-2", payload+" two"
	return []archive.Metadata{spec.build(), other.build()}
}

// assertInert fails when what a payload would do, if it reached the page as
// markup, is in it: the payload's dangerous fragments raw, or a character a
// name may not carry.
func assertInert(t *testing.T, page, payload string) {
	t.Helper()
	for _, raw := range []string{
		"</script><script>", "<img src=x", "<svg onload", "onerror=alert(1)>", "</style><style>", "</title></head>",
		"\xe2\x80\xa8", "\xe2\x80\xa9", "\xe2\x80\xae", "\xe2\x80\xac", "\x1b", "\x00", "\x07", "\x7f", "\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd",
	} {
		if strings.Contains(page, raw) {
			t.Errorf("payload %q: the page contains %q", payload, raw)
		}
	}
	if strings.Contains(payload, "<") && !strings.Contains(page, "&lt;") {
		t.Errorf("payload %q: the page has no escaped < at all, so the name was dropped or not escaped", payload)
	}
}

// A name the page shows is shortened, so a name of any length cannot stretch
// the page.
func TestLongNamesAreShortened(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	out := string(render(t, computeFixture(t, hostileSessions(long), 30, stats.GroupNone), Options{IncludeProjectNames: true}))
	if strings.Contains(out, strings.Repeat("x", nameLimit+1)) {
		t.Error("a 5,000 character name is not shortened")
	}
	if !strings.Contains(out, strings.Repeat("x", nameLimit-1)+"…") {
		t.Error("a shortened name does not end in an ellipsis")
	}
}

func TestPageIsSelfContainedInBothThemes(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupWeek), Options{}))
	for _, want := range []string{
		`name="viewport"`, `prefers-color-scheme: dark`, `@media print`, `forced-colors: active`, `max-width: 559px`,
		`Content-Security-Policy`, `default-src &#39;none&#39;`, `lang="en"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(out, "script-src") || strings.Contains(out, "connect-src") {
		t.Error("the policy opens something it should not")
	}
}

// The chart names its peak, every bar answers a hover, and a day with sessions
// but no token counts is marked, not drawn as zero.
func TestDailyChartLabelsThePeakAndAnswersHover(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	if !strings.Contains(out, "Peak 29M · Sep 17") {
		t.Error("the chart does not label its peak")
	}
	if got := strings.Count(out, `<g class="slot"><title>`); got != 30 {
		t.Errorf("%d hover titles, want one per day (30)", got)
	}
	for _, want := range []string{
		"<title>Sep 17 · 29M tokens · 2 sessions</title>",
		"<title>Sep 3 · no sessions</title>",
		"<title>Sep 15 · 1 session · token count unknown</title>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the chart lacks the hover text %q", want)
		}
	}
	if !strings.Contains(out, `class="bar unknown"`) || !strings.Contains(out, `class="bar day peak"`) {
		t.Error("the chart does not mark an unknown day and the peak")
	}
	if !strings.Contains(out, `role="img" aria-label="Tokens by day, Aug 31 to Sep 29. Peak 29M tokens on Sep 17."`) {
		t.Error("the chart has no text alternative naming the peak")
	}
	if !strings.Contains(out, "<summary>Show as a table</summary>") {
		t.Error("the chart has no table view")
	}
}

// A long window draws at most maxBars bars, each the busiest day of its run,
// and the page stays small however large the archive.
func TestLargeArchiveStaysWithinItsBudget(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	start := fixtureNow.AddDate(-9, 0, 0)
	for i := range 6000 {
		sessions = append(sessions, sessionSpec{
			id: fmt.Sprintf("s%05d", i), harness: []string{"claude", "codex", "cursor"}[i%3],
			project:  fmt.Sprintf("project-%d-%s", i%700, strings.Repeat("n", i%50)),
			captured: start.Add(time.Duration(i) * 13 * time.Hour), models: []string{"claude-opus-5", fmt.Sprintf("model-%d", i%40)},
			turns: 5, messages: 30, toolResults: 20, errors: 1, skills: []string{fmt.Sprintf("skill-%d", i%60)},
			mcp:    map[string]int{fmt.Sprintf("server-%d", i%30): 4},
			tokens: []tokenSpec{{fmt.Sprintf("model-%d", i%40), 10_000 + i, 5_000, 200_000, 20_000}},
		}.build())
	}
	for _, by := range []stats.Grouping{stats.GroupNone, stats.GroupDay, stats.GroupProject} {
		s := computeFixture(t, sessions, stats.MaxDays, by)
		out := render(t, s, Options{IncludeProjectNames: true})
		if len(out) > 300_000 {
			t.Errorf("by %q: the page is %d bytes, over the 300,000 budget", by, len(out))
		}
		if bars := strings.Count(string(out), `<g class="slot">`); bars > maxBars {
			t.Errorf("by %q: %d bars, over %d", by, bars, maxBars)
		}
		if !strings.Contains(string(out), "Each bar is 31 days and shows its busiest day.") {
			t.Errorf("by %q: the chart does not say what a bar is", by)
		}
	}
}

// Cursor records no tokens: its tokens and cost read unknown and n/a, never 0,
// and a window with no token data says so instead of drawing an empty chart.
func TestUnknownTokensAreNeverZero(t *testing.T) {
	t.Parallel()
	cursor := sessionSpec{id: "c1", harness: "cursor", project: "p", captured: day(time.September, 20, 10), models: []string{"cursor-auto"}, turns: 3}
	s := computeFixture(t, []archive.Metadata{cursor.build()}, 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	for _, want := range []string{"unknown", "n/a", "No session in this window reports token counts"} {
		if !strings.Contains(out, want) {
			t.Errorf("the Cursor-only page lacks %q", want)
		}
	}
	for _, bad := range []string{"Tokens by day, ", "What used your tokens", ">0 tokens", "$0"} {
		if strings.Contains(out, bad) {
			t.Errorf("the Cursor-only page contains %q", bad)
		}
	}
	mixed := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	if !strings.Contains(mixed, `<td class="num">3 (14%)</td><td class="num">unknown</td><td class="num">n/a</td>`) {
		t.Error("the agents table does not show Cursor as unknown tokens and n/a cost")
	}
}

func TestEmptyWindowGetsAHelpfulPage(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, nil, 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	for _, want := range []string{"Nothing to show yet", "Try a longer window", "This page holds counts and names only"} {
		if !strings.Contains(out, want) {
			t.Errorf("the empty page lacks %q", want)
		}
	}
	custom := string(render(t, s, Options{EmptyMessage: "No archived sessions match <these> filters."}))
	if !strings.Contains(custom, "No archived sessions match &lt;these&gt; filters.") {
		t.Error("the caller's empty message is not shown, escaped")
	}
	if strings.Contains(out, "Tokens by day") || strings.Contains(out, `class="card stat"`) {
		t.Error("the empty page draws sections without data")
	}
}

// The composition donut's segments add up to the whole ring, a segment with
// no tokens is not drawn, and every segment is in the legend with its share.
func TestDonutSegmentsAddUp(t *testing.T) {
	t.Parallel()
	s := computeFixture(t, fixtureSessions(), 30, stats.GroupNone)
	out := string(render(t, s, Options{}))
	circ := regexp.MustCompile(`stroke-dasharray="([0-9.]+) ([0-9.]+)"`)
	matches := circ.FindAllStringSubmatch(out, -1)
	if len(matches) != 4 {
		t.Fatalf("%d ring segments, want 4", len(matches))
	}
	for _, m := range matches {
		var a, b float64
		if _, err := fmt.Sscanf(m[1]+" "+m[2], "%g %g", &a, &b); err != nil {
			t.Fatal(err)
		}
		if total := a + b; total < 427.2 || total > 427.3 {
			t.Errorf("segment %s + %s = %g, want the circumference 427.26", m[1], m[2], total)
		}
	}
	for _, label := range []string{"Cache read", "Cache write", "Input", "Output"} {
		if !strings.Contains(out, ">"+label+"</th>") && !strings.Contains(out, "</svg>"+label+"</th>") {
			t.Errorf("the legend lacks %q", label)
		}
	}
	c := s.Composition
	// A composition of one type is a whole ring, without a gap.
	only := *c
	only.CacheRead = stats.Segment{Tokens: c.Total, Share: 1}
	only.CacheWrite, only.FreshInput, only.Output = stats.Segment{}, stats.Segment{}, stats.Segment{}
	s.Composition = &only
	single := string(render(t, s, Options{}))
	if got := strings.Count(single, "stroke-dasharray="); got != 1 {
		t.Errorf("%d ring segments for a single-type composition, want 1", got)
	}
	if !strings.Contains(single, `stroke-dasharray="427.26 0"`) {
		t.Error("a single-type composition is not a whole ring")
	}
}

// The colors clear WCAG: text at 4.5:1 and marks at 3:1 against the surface,
// in both themes, and the categorical ring colors are the ones the palette
// was validated with.
// reliefColors are the marks the validated palette leaves under 3:1 against
// the surface, by theme; they are ring segments whose share and tokens are
// also written out in the legend.
var reliefColors = map[string]map[string]bool{"light": {"seg3": true, "seg4": true}}

func TestPaletteContrast(t *testing.T) {
	t.Parallel()
	light, dark := themeColors(t)
	for name, theme := range map[string]map[string]string{"light": light, "dark": dark} {
		surface := theme["surface"]
		for _, text := range []string{"ink", "ink-2", "ink-3"} {
			if got := contrast(t, theme[text], surface); got < 4.5 {
				t.Errorf("%s: --%s on the surface is %.2f:1, under 4.5:1", name, text, got)
			}
			if got := contrast(t, theme[text], theme["page"]); got < 4.5 {
				t.Errorf("%s: --%s on the page is %.2f:1, under 4.5:1", name, text, got)
			}
		}
		for _, mark := range []string{"series", "series-strong", "seg1", "seg2", "seg3", "seg4", "unknown"} {
			floor := 3.0
			// The light theme's aqua and yellow sit under 3:1 by design of the
			// validated palette; the legend and table carry them (relief rule).
			if reliefColors[name][mark] {
				floor = 2.0
			}
			if got := contrast(t, theme[mark], surface); got < floor {
				t.Errorf("%s: --%s against the surface is %.2f:1, under %.1f:1", name, mark, got, floor)
			}
		}
		// Labels on the ring are black; each ring color must carry them.
		for _, seg := range []string{"seg1", "seg2", "seg3", "seg4"} {
			if got := contrast(t, "#000000", theme[seg]); got < 4.5 {
				t.Errorf("%s: black label on --%s is %.2f:1, under 4.5:1", name, seg, got)
			}
		}
	}
	if light["seg1"] != "#2a78d6" || dark["seg1"] != "#3987e5" {
		t.Error("the first ring color is not the validated palette's slot 1")
	}
}

// Delta glyphs come with a spoken form, and a number is never a color alone:
// every up or down arrow has its words next to it for a screen reader.
func TestDeltasHaveASpokenForm(t *testing.T) {
	t.Parallel()
	out := string(render(t, computeFixture(t, fixtureSessions(), 30, stats.GroupNone), Options{}))
	if !strings.Contains(out, `<span aria-hidden="true">▲ 267%</span><span class="sr">up 267 percent</span>`) {
		t.Error("the sessions delta has no spoken form")
	}
}

// The renderer is a pure function of the stats: it reads no clock, file,
// environment or network, so it can never fetch or write anything itself.
func TestRendererImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/statshtml")
	importgraph.Forbid(t, "internal/statshtml", direct,
		"os", "os/exec", "io/ioutil", "io/fs", "path/filepath", "net", "net/http", "math/rand", "math/rand/v2",
		"golang.org/x/term", "text/template", "unsafe")
	importgraph.Forbid(t, "internal/statshtml (transitively)", all, "net/http", "os/exec")
	for _, path := range all {
		if strings.HasPrefix(path, "github.com/wangjohn/agent-archive/internal/") &&
			path != "github.com/wangjohn/agent-archive/internal/archive" &&
			path != "github.com/wangjohn/agent-archive/internal/stats" {
			t.Errorf("internal/statshtml reaches %s; only internal/stats and internal/archive are allowed", path)
		}
	}
}

// A value typed as trusted markup skips the template's escaping and would let
// a name through as markup, so nothing in the renderer is: text is strings.
// The stylesheet is the template's own text, and holds no template action.
func TestNothingIsMarkedTrusted(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"model.go", "render.go", "charts.go", "format.go"} {
		src := readSource(t, file)
		for _, unsafe := range []string{"template.HTML", "template.JS", "template.URL", "template.HTMLAttr", "template.JSStr", "template.Srcset", "template.CSS"} {
			if strings.Contains(src, unsafe) {
				t.Errorf("%s uses %s: a value marked trusted skips escaping", file, unsafe)
			}
		}
	}
	if strings.Contains(pageTemplateSource, "safeHTML") || strings.Contains(pageTemplateSource, "{{define \"script") {
		t.Error("the template opts out of escaping")
	}
	if strings.Contains(styleSheet, "{{") {
		t.Error("the stylesheet holds a template action")
	}
}
