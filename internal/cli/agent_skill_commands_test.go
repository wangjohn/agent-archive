package cli

import (
	"flag"
	"regexp"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentskills"
)

// skillQuotedCode is the code a rendered skill quotes: its indented command
// lines, its inline code spans, and the rules of its allowed-tools field.
func skillQuotedCode(text string) []string {
	var code []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "    ") {
			code = append(code, strings.TrimSpace(line))
		}
		if rules, ok := strings.CutPrefix(line, "allowed-tools: "); ok {
			for _, m := range regexp.MustCompile(`Bash\(([^)]*)\)`).FindAllStringSubmatch(rules, -1) {
				code = append(code, strings.TrimSuffix(m[1], ":*"))
			}
		}
	}
	return append(code, quotedCode(text)...)
}

// quotedString is a double-quoted argument, which the docs' scan stops at.
var quotedString = regexp.MustCompile(`"[^"\n]*"`)

// skillInvocationProblems is quotedInvocationProblems for a skill's code,
// with each quoted argument (the words to search for) as one plain word so
// that the flags after it are checked too.
func skillInvocationProblems(sets map[string]*flag.FlagSet, code string) (problems []string, checked int) {
	return quotedInvocationProblems(sets, quotedString.ReplaceAllString(code, "TITLE"), false)
}

// Every command and flag a skill tells its agent to run exists in the CLI:
// a renamed or removed flag cannot linger in text every installed agent
// reads. The skills are rendered as if the executable were on PATH, so the
// same scan as the docs' finds the commands.
func TestAgentSkillsQuoteOnlyRealCommandsAndFlags(t *testing.T) {
	t.Parallel()
	sets := commandFlagSets(t)
	checked := 0
	for _, skill := range agentskills.Registry {
		for _, dest := range []agentskills.Destination{agentskills.Claude, agentskills.Shared} {
			text := string(skill.Render(dest, "agent-archive", ""))
			for _, code := range skillQuotedCode(text) {
				problems, n := skillInvocationProblems(sets, code)
				checked += n
				for _, problem := range problems {
					t.Errorf("%s skill (%d): %s", skill.Name, dest, problem)
				}
			}
		}
	}
	if checked < 12 {
		t.Fatalf("checked only %d flags in the skills; the extraction is broken", checked)
	}
}

// The check catches what it is for: a command or flag the CLI does not have,
// and a line of prose that is not a command at all.
func TestAgentSkillCommandCheckCatchesAWrongCommand(t *testing.T) {
	t.Parallel()
	sets := commandFlagSets(t)
	for code, want := range map[string]string{
		"agent-archive handoff \"x\" --harnes codex": "no flag --harnes",
		"agent-archive list --json --recent":         "no flag --recent",
		"agent-archive find \"x\"":                   `no command "find"`,
		"agent-archive show ID --transcripts":        "no flag --transcripts",
	} {
		problems, _ := skillInvocationProblems(sets, code)
		if len(problems) != 1 || !strings.Contains(problems[0], want) {
			t.Errorf("%q: problems = %v, want one naming %q", code, problems, want)
		}
	}
	if problems, checked := skillInvocationProblems(sets, "agent-archive handoff \"x\" --harness codex"); len(problems) != 0 || checked != 1 {
		t.Errorf("a real command: problems=%v checked=%d", problems, checked)
	}
}
