package cli

import (
	"fmt"
	"strconv"
	"strings"
)

func (p *prompter) setupStep(n int, title string) {
	if !p.singleArea {
		title = fmt.Sprintf("Step %d of 3 · %s", n, title)
	}
	p.renderer().block(p.style.bold(title) + "\n")
}
func (p *prompter) setupHeading(title string) {
	p.renderer().block(p.style.bold(strings.TrimSpace(title)) + "\n")
}
func (p *prompter) setupMenu(question, def string, choices ...option) (string, error) {
	return p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Default: def, Primary: choices})
}
func (p *prompter) setupActions(question, def string, primary []option, secondary []actionOption, aliases ...option) (string, error) {
	var actions []actionOption
	for _, a := range secondary {
		if a.Shortcut == "" {
			primary = append(primary, option{a.Key, a.Label})
		} else {
			actions = append(actions, a)
		}
	}
	return p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Default: def, Primary: primary, Secondary: actions, Aliases: aliases})
}
func (p *prompter) setupYesNo(question string, def bool) (bool, error) {
	key := "no"
	if def {
		key = "yes"
	}
	answer, err := p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Default: key, Primary: []option{{"yes", "Yes"}, {"no", "No"}}, Aliases: []option{{"y", ""}, {"n", ""}}, ResolveReceipt: func(value string) string {
		if value == "y" || value == "yes" {
			return "Yes"
		}
		return "No"
	}})
	return answer == "yes" || answer == "y", err
}
func (p *prompter) setupText(label, def string) (string, error) {
	return p.guidedText(promptModel{Question: strings.TrimSpace(label), Default: def, Label: "Answer", Receipt: strings.TrimSpace(label)})
}
func (p *prompter) setupRequired(label, def string) (string, error) {
	return p.guidedText(promptModel{Question: strings.TrimSpace(label), Default: def, Label: "Answer", Receipt: strings.TrimSpace(label), Validate: func(value string) error {
		if value == "" {
			return fmt.Errorf("this value is required")
		}
		return nil
	}})
}

func (p *prompter) setupRetentionDays(def int) (int, error) {
	answer, err := p.guidedText(promptModel{Question: "Keep sessions for how many days?", Label: "Days", Default: fmt.Sprint(def), Receipt: "Retention", Validate: func(value string) error {
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 36500 {
			return fmt.Errorf("enter a number of days between 1 and 36500")
		}
		return nil
	}})
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(answer)
}

// Storage field actions use a reserved prefix so valid bucket/profile names
// remain literal, including "b" and "back".
func (p *prompter) setupStorageRequired(label, def string) (string, error) {
	value, err := p.guidedText(promptModel{Question: label, Helpers: []string{"[:back] Back to storage options"}, Label: "Answer", Default: def, Receipt: label, Validate: func(value string) error {
		if value == "" {
			return fmt.Errorf("this value is required")
		}
		return nil
	}})
	if err == nil && value == ":back" {
		return "", errChooseStorageAgain
	}
	return value, err
}
