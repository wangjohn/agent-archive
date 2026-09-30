package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

// hostileNames are what a transcript could carry as a project, model, skill
// or MCP server name: terminal sequences, bidirectional controls, invisible
// and wide characters, marks that add no width, and names far wider than any
// column.
var hostileNames = []string{
	"proj\x1b[31mRED\x1b]0;title\x07",
	"\x1b[2J\x1b[Hgone",
	"abc\u202edef\u2066ghi\u2069",
	"zero\u200bwidth\u200d\ufeffjoin",
	"c1\u009b31mred\u0085next",
	"line1\nline2\r\nline3\ttab",
	"日本語のとても長いプロジェクト名前です日本語のとても長いプロジェクト名前です",
	"e" + strings.Repeat("\u0301", 200),
	strings.Repeat("w", 300),
	"emoji 🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂🙂",
	"bad\xff\xfeutf8",
	"del\x7fchar\x00nul",
	"\u2028sep\u2029",
	// A narrow character and U+FE0F is a two-column emoji.
	"hearts " + strings.Repeat("\u2764\ufe0f", 40),
	"\U0001F5A5\ufe0fdesk\u2714\ufe0f",
}

// Whatever names the archive carries, no line of the screen is wider than
// the terminal, and none carries a control character, a terminal escape or a
// bidirectional override: at every width, in every layout, in text and JSON.
func TestStatsHostileNamesNeverBreakTheScreen(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	for i, name := range hostileNames {
		syntheticSession{
			id: fmt.Sprintf("hostile-%02d", i), harness: "claude", project: name, captured: statsDay(time.September, 20+i%8, 9),
			models: []string{name}, turns: 2, messages: 4, toolResults: 10, errors: 1, skills: []string{name}, mcp: map[string]int{name: 3},
			perModel: []modelTokenSpec{{name, 1000 * (i + 1), 500, 200, 100}},
		}.publish(t, mem)
	}
	syntheticSession{
		id: "cursor", harness: "cursor", project: hostileNames[6], captured: statsDay(time.September, 28, 9), models: []string{hostileNames[6]}, turns: 1,
	}.publish(t, mem)
	for _, width := range []int{statsMinWidth, 51, 60, 79, 80, 81, 100, 120, 250} {
		for _, args := range [][]string{nil, {"--by", "project"}, {"--by", "day", "--days", "14"}} {
			out := mustRunStats(t, env, width, args...)
			checkTerminalSafe(t, fmt.Sprintf("width %d %v", width, args), out, width)
		}
	}
	checkTerminalSafe(t, "not a terminal", mustRunStats(t, env, 0), statsUnknownWidth)
	checkTerminalSafe(t, "json", mustRunStats(t, env, 0, "--json", "--by", "project"), 1<<20)
}

// checkTerminalSafe fails when out has a line wider than width columns or a
// character a terminal would act on.
func checkTerminalSafe(t *testing.T, where, out string, width int) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if w := visibleWidth(line); w > width {
			t.Errorf("%s: line is %d columns, over %d: %q", where, w, width, line)
		}
		for _, r := range line {
			switch {
			case r == unicode.ReplacementChar:
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f), r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x2028 || r == 0x2029:
				t.Errorf("%s: control character %U in %q", where, r, line)
			}
		}
	}
}

// A price file's currency and version are printed on the screen, so a file
// whose currency is not a three-letter code, or whose version is a paragraph,
// is refused as a usage error; any code that is accepted fits the terminal.
func TestStatsPriceFileLabelsCannotBreakTheScreen(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		currency string
		version  string
		ok       bool
	}{
		{"EUR", "v1", true}, {"zzz", "v1", true}, {"", "v1", true},
		{"ABCDEFGHIJ", "v1", false}, {strings.Repeat("X", 60), "v1", false}, {"\u20ac\x1b[31m$$", "v1", false},
		{"USD", strings.Repeat("v", 300), false},
	} {
		file := filepath.Join(t.TempDir(), "prices.json")
		currency, _ := json.Marshal(tc.currency)
		version, _ := json.Marshal(tc.version)
		custom := fmt.Sprintf(`{"version": %s, "currency": %s, "as_of": "2026-09-30", "models": [
	  {"id": "claude-opus-5", "family": "opus", "input_per_mtok": 5, "output_per_mtok": 25, "cache_read_per_mtok": 0.5, "cache_write_per_mtok": 6.25},
	  {"id": "claude-sonnet-5", "family": "sonnet", "input_per_mtok": 2, "output_per_mtok": 10, "cache_read_per_mtok": 0.2, "cache_write_per_mtok": 2.5},
	  {"id": "gpt-5", "family": "gpt-5", "input_per_mtok": 1.25, "output_per_mtok": 10, "cache_read_per_mtok": 0.125, "cache_write_per_mtok": 0}]}`, version, currency)
		if err := os.WriteFile(file, []byte(custom), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, width := range []int{statsMinWidth, 60, 80, 100} {
			out, errOut, code := runStats(t, env, width, "--prices", file, "--by", "week")
			if !tc.ok {
				if code != 2 || out != "" || !strings.Contains(errOut, "--prices") {
					t.Fatalf("%q/%d: code=%d stdout=%q stderr=%q, want a usage error", tc.currency, len(tc.version), code, out, errOut)
				}
				continue
			}
			if code != 0 {
				t.Fatalf("%q: code=%d stderr=%q", tc.currency, code, errOut)
			}
			checkTerminalSafe(t, fmt.Sprintf("currency %q width %d", tc.currency, width), out, width)
		}
	}
}
