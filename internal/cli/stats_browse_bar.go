package cli

import (
	"strconv"
	"strings"
)

// statsBarItem is one key of the key bar. Its label has four forms, from the
// full one to the shortest: the bar takes the longest set that fits the
// terminal.
type statsBarItem struct {
	key    rune
	labels [4]string
}

// statsBarItems are the bar's keys in order. The window's label takes the
// window's days: "%s" stands for them.
var statsBarItems = []statsBarItem{
	{'o', [4]string{"overview", "overview", "over", ""}},
	{'d', [4]string{"detail", "detail", "detail", ""}},
	{'p', [4]string{"projects", "projects", "proj", ""}},
	{'m', [4]string{"models", "models", "model", ""}},
	{'a', [4]string{"agents", "agents", "agent", ""}},
	{'w', [4]string{"window %s", "%s", "%s", "%s"}},
	{'h', [4]string{"html", "html", "html", "html"}},
	{'?', [4]string{"help", "help", "help", "help"}},
	{'q', [4]string{"quit", "quit", "quit", "quit"}},
}

// statsBarStep is one way to lay the bar out: which label form, the gap
// between items, and which keys.
type statsBarStep struct {
	labels int
	gap    int
	keys   string
}

// statsBarSteps are the layouts tried in order, until one fits the terminal:
// the full bar, then tighter, then shorter labels, then fewer keys. The last
// keys are the ones that can never be dropped: quit.
var statsBarSteps = []statsBarStep{
	{0, 2, "odpmawh?q"},
	{0, 1, "odpmawh?q"},
	{1, 1, "odpmawh?q"},
	{2, 1, "odpmawh?q"},
	{2, 1, "odpmaw?q"},
	{3, 1, "odpmaw?q"},
	{3, 1, "w?q"},
	{3, 1, "?q"},
	{3, 1, "q"},
}

// statsKeyBar is the last row of the interactive screen: the keys, the active
// view marked and the window in days on the w key. It is never wider than
// width: the labels shorten and keys drop as the terminal narrows, and what
// is left is cut as a last resort, so it never wraps.
//
// Keys are bold and labels dim; the active view is in reverse video, or
// in brackets when there is no color, so it does not rely on color.
func statsKeyBar(style textStyle, active statsPage, window, width int) string {
	days := strconv.Itoa(window) + "d"
	var bar string
	for _, step := range statsBarSteps {
		bar = layStatsBar(style, active, days, step)
		if visibleWidth(bar) <= width {
			return bar
		}
	}
	return truncateVisible(bar, width)
}

// layStatsBar lays the bar out as one step says.
func layStatsBar(style textStyle, active statsPage, days string, step statsBarStep) string {
	var bar strings.Builder
	first := true
	for _, item := range statsBarItems {
		if !strings.ContainsRune(step.keys, item.key) {
			continue
		}
		if first {
			bar.WriteString(" ")
		} else {
			bar.WriteString(strings.Repeat(" ", step.gap))
		}
		first = false
		label := strings.ReplaceAll(item.labels[step.labels], "%s", days)
		bar.WriteString(statsBarCell(style, item, label, statsViewKeys[item.key] == active))
	}
	return bar.String()
}

// statsBarCell is one item of the bar: its key and its label, marked when it
// is the view on show.
func statsBarCell(style textStyle, item statsBarItem, label string, active bool) string {
	plain := string(item.key)
	if label != "" {
		plain += " " + label
	}
	switch {
	case active && style.color:
		return style.paint(statsRoleCodes[roleKeyActive], plain)
	case active:
		return "[" + plain + "]"
	case label == "":
		return style.bold(plain)
	}
	return style.bold(string(item.key)) + " " + style.dim(label)
}
