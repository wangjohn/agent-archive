package cli

import (
	"strconv"
	"strings"
)

// statsBarItem is one key of the key bar. Its label has two forms, the full
// one and a short one; the window key's label is made by windowLabel.
type statsBarItem struct {
	key   rune
	label [2]string
}

// statsBarItems are the bar's keys in order.
var statsBarItems = []statsBarItem{
	{'o', [2]string{"overview", "over"}},
	{'d', [2]string{"detail", "detail"}},
	{'p', [2]string{"projects", "proj"}},
	{'m', [2]string{"models", "model"}},
	{'a', [2]string{"agents", "agent"}},
	{'w', [2]string{}},
	{'h', [2]string{"html", "html"}},
	{'?', [2]string{"help", "help"}},
	{'q', [2]string{"quit", "quit"}},
}

// statsBarStep is one way to lay the bar out: the label form of the views and
// of the window key, the gap between items, and which keys.
type statsBarStep struct {
	views  int
	window int
	gap    int
	keys   string
}

// statsBarSteps are the layouts tried in order, until one fits the terminal:
// the full bar, then tighter, then shorter labels, then fewer keys. The last
// keys are the ones that can never be dropped: quit.
var statsBarSteps = []statsBarStep{
	{0, 0, 2, "odpmawh?q"},
	{0, 0, 1, "odpmawh?q"},
	{0, 1, 1, "odpmawh?q"},
	{0, 2, 1, "odpmawh?q"},
	{1, 1, 1, "odpmawh?q"},
	{1, 2, 1, "odpmawh?q"},
	{1, 2, 1, "odpmaw?q"},
	{2, 2, 1, "odpmaw?q"},
	{2, 2, 1, "w?q"},
	{2, 2, 1, "?q"},
	{2, 2, 1, "q"},
}

// windowLabel is the window key's label: what it moves the window to (the
// window on show is in the screen's title), in the form the step asks for,
// from "window 30d>90d" to just ">90d".
func windowLabel(form int, days, next string) string {
	switch form {
	case 0:
		return "window " + days + ">" + next
	case 1:
		return days + ">" + next
	}
	return ">" + next
}

// statsKeyBar is the last row of the interactive screen: the keys, the active
// view marked and the window key saying which window it moves to. It is never
// wider than width: the labels shorten and keys drop as the terminal narrows,
// and what is left is cut as a last resort, so it never wraps.
//
// Keys are bold and labels dim; the active view is in reverse video, or
// in brackets when there is no color, so it does not rely on color.
func statsKeyBar(style textStyle, active statsPage, window, next, width int) string {
	days, target := strconv.Itoa(window)+"d", strconv.Itoa(next)+"d"
	var bar string
	for _, step := range statsBarSteps {
		bar = layStatsBar(style, active, days, target, step)
		if visibleWidth(bar) <= width {
			return bar
		}
	}
	return truncateVisible(bar, width)
}

// layStatsBar lays the bar out as one step says.
func layStatsBar(style textStyle, active statsPage, days, next string, step statsBarStep) string {
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
		bar.WriteString(statsBarCell(style, item, itemLabel(item, step, days, next), statsViewKeys[item.key] == active))
	}
	return bar.String()
}

// itemLabel is item's label in the form step asks for. The keys that are not
// views (h, ? and q) keep theirs when the views' are left out.
func itemLabel(item statsBarItem, step statsBarStep, days, next string) string {
	switch {
	case item.key == 'w':
		return windowLabel(step.window, days, next)
	case step.views == 0:
		return item.label[0]
	case step.views == 1 || strings.ContainsRune("h?q", item.key):
		return item.label[1]
	}
	return ""
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
