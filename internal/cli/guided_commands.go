package cli

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"io"
	"strconv"
	"strings"
)

// guidedMenu uses shared blocks while preserving command-specific option keys.
func (p *prompter) guidedMenu(question, def string, choices ...option) (string, error) {
	primary := []option{}
	secondary := []actionOption{}
	for _, o := range choices {
		switch o.Key {
		case "cancel":
			secondary = append(secondary, actionOption{o.Key, "q", o.Label})
		case "done":
			secondary = append(secondary, actionOption{o.Key, "d", o.Label})
		default:
			primary = append(primary, o)
		}
	}
	return p.guidedChoice(promptModel{Question: question, Default: def, Primary: primary, Secondary: secondary})
}

// guidedYesNo keeps destructive decisions opt-in and resolves named answers.
func (p *prompter) guidedYesNo(question string, def bool) (bool, error) {
	key := "no"
	if def {
		key = "yes"
	}
	answer, err := p.guidedChoice(promptModel{Question: question, Default: key, Primary: []option{{"yes", "Yes"}, {"no", "No"}}})
	return answer == "yes", err
}

func (p *prompter) guidedDefault(question, def string) (string, error) {
	return p.guidedText(promptModel{Question: question, Default: def})
}

// guidedRetention keeps the existing bounds on a standalone import edit.
func (p *prompter) guidedRetention(def int) (int, error) {
	value, err := p.guidedText(promptModel{Question: "Keep sessions for how many days?", Default: strconv.Itoa(def), Validate: func(value string) error {
		days, err := strconv.Atoi(value)
		if err != nil || days <= 0 || days > 36500 {
			return errors.New("Enter a number of days between 1 and 36500.")
		}
		return nil
	}})
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(value)
}

// guidedRows aligns human records and switches to labeled fields when a table
// would exceed the available terminal width. Structured output bypasses it.
func guidedRows(out io.Writer, headers []string, rows [][]string) {
	caps := capabilitiesFor(nil, out)
	width := caps.Width
	if width <= 0 {
		width = 80
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleWidth(h)
	}
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], visibleWidth(value))
		}
	}
	total := 2 * (len(headers) - 1)
	for _, n := range widths {
		total += n
	}
	if total <= width {
		render := func(row []string) {
			for i, value := range row {
				terminal.Print(out, value)
				if i+1 < len(row) {
					terminal.Print(out, strings.Repeat(" ", widths[i]-visibleWidth(value)+2))
				}
			}
			terminal.Println(out)
		}
		render(headers)
		for _, row := range rows {
			render(row)
		}
		return
	}
	for n, row := range rows {
		if n > 0 {
			terminal.Println(out)
		}
		labelWidth := 0
		for _, h := range headers {
			labelWidth = max(labelWidth, visibleWidth(h))
		}
		for i, value := range row {
			prefix := "  " + headers[i] + strings.Repeat(" ", labelWidth-visibleWidth(headers[i])+2)
			if width < 60 {
				terminal.Println(out, "  "+headers[i])
				prefix = "    "
			}
			terminal.Println(out, hangingIndent(prefix, value, width))
		}
	}
	if len(rows) == 0 {
		terminal.Println(out, "No records found.")
	}
}

// guidedExplanation sets off a command's plan or consequences before consent.
func guidedExplanation(out io.Writer, title string, lines ...string) {
	width := capabilitiesFor(nil, out).Width
	if width <= 0 {
		width = 80
	}
	terminal.Println(out, styleFor(out).bold(title))
	terminal.Println(out)
	for _, line := range lines {
		terminal.Println(out, hangingIndent("", line, width))
	}
}
