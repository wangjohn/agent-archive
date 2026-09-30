package cli

import "strings"

// The stats screen's color table: the only place a color is chosen. Every
// entry is a code from the terminal's own 16 ANSI colors (or bold or dim), so
// the screen follows the user's theme; no 256-color or truecolor code is ever
// written. A color is a role, never the only carrier of a meaning: each
// colored element also has a label, a glyph or a position that says the same
// (see statsColorsHaveNonColorCues in the tests).
//
//	agents        Claude Code yellow, Cursor blue, Codex green
//	model family  opus magenta, fable bright red, sonnet cyan, haiku yellow,
//	              gpt and codex models green
//	projects      cyan
//	daily spend   cyan
//	deltas        spend up yellow (amber), down green; never red
//	heads-up      yellow bullets
//	token types   cache read blue, cache write yellow, input green, output magenta
//	labels        dim; emphasis bold
//
// Yellow carries three meanings (Claude Code, a heads-up bullet and a rise in
// spend, and the haiku and cache-write colors on other screens). They are far
// apart on a screen and each has a label or a glyph beside it. Only marks are
// yellow (a bullet, a bar, an arrow with its figure), never a sentence or an
// amount, because yellow text is hard to read on a light background.
//
// Dim (SGR 2) rather than bright black is the grey: bright black is the
// background itself in some popular themes (Solarized), and the rest of the
// command line already uses dim for secondary text.

// statsRole names what a piece of the screen means.
type statsRole int

// The roles of the stats screen.
const (
	// roleEmphasis is a title or a headline number.
	roleEmphasis statsRole = iota
	// roleLabel is a caption, a unit, a note: secondary text.
	roleLabel
	// roleProject is a project's bar.
	roleProject
	// roleSpendChart is the daily spend chart's bars.
	roleSpendChart
	// roleHeadsUp is a heads-up note's bullet.
	roleHeadsUp
	// roleDeltaUp is a rise in spend: amber, never red.
	roleDeltaUp
	// roleDeltaDown is a fall in spend.
	roleDeltaDown
	// roleCacheRead and the roles after it are the four kinds of token.
	roleCacheRead
	roleCacheWrite
	roleFreshInput
	roleOutput
)

// statsRoleCodes is the SGR code of each role.
var statsRoleCodes = map[statsRole]string{
	roleEmphasis:   "1",
	roleLabel:      "2",
	roleProject:    "36",
	roleSpendChart: "36",
	roleHeadsUp:    "33",
	roleDeltaUp:    "33",
	roleDeltaDown:  "32",
	roleCacheRead:  "34",
	roleCacheWrite: "33",
	roleFreshInput: "32",
	roleOutput:     "35",
}

// statsAgentCodes is the color of each agent, by the archive's harness name.
// An agent the table does not know is drawn without color.
var statsAgentCodes = map[string]string{
	"claude": "33",
	"cursor": "34",
	"codex":  "32",
}

// statsModelCodes is the color of each model family, matched as a prefix of
// the family's label ("opus", "gpt-5", "claude-opus-5" when unpriced). The
// first prefix that matches wins. A family the table does not know is drawn
// without color.
var statsModelCodes = []struct {
	prefix string
	code   string
}{
	{"opus", "35"},
	{"fable", "91"},
	{"sonnet", "36"},
	{"haiku", "33"},
	{"gpt", "32"},
	{"codex", "32"},
}

// statsFamilyOf reduces a model label to the family a color goes by: a full
// Claude model id ("claude-opus-5") to "opus".
func statsFamilyOf(label string) string {
	lower := strings.ToLower(label)
	lower = strings.TrimPrefix(lower, "claude-")
	return lower
}

// modelCode is the color of a model family's label, or "" for none.
func modelCode(label string) string {
	family := statsFamilyOf(label)
	for _, m := range statsModelCodes {
		if strings.HasPrefix(family, m.prefix) {
			return m.code
		}
	}
	return ""
}

// agentCode is the color of an agent, or "" for none.
func agentCode(harness string) string { return statsAgentCodes[harness] }

// role paints text in a role's color.
func (p *statsPrinter) role(r statsRole, text string) string {
	return p.v.style.paint(statsRoleCodes[r], text)
}

// paint paints text in an SGR code; an empty code leaves it as it is.
func (p *statsPrinter) paint(code, text string) string {
	if code == "" {
		return text
	}
	return p.v.style.paint(code, text)
}
