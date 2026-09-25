package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// cliReferencePath is the generated reference, from this package's directory.
var cliReferencePath = filepath.Join("..", "..", "docs", "reference", "cli.md")

// commandFlagSets returns every public command's own flag set ("backfill
// undo" included), caught as the command builds it: each command is run with
// an unknown flag, which it rejects before doing anything else.
func commandFlagSets(t *testing.T) map[string]*flag.FlagSet {
	t.Helper()
	sets := map[string]*flag.FlagSet{}
	env := testEnv(t, t.TempDir(), time.Now())
	for command := range commandHelp {
		if command == "version" {
			continue // takes no flags; it has no flag set
		}
		var built []*commandFlags
		env.observeFlags = func(f *commandFlags) { built = append(built, f) }
		args := append(strings.Fields(command), "--no-such-flag")
		if code := Run(args, nil, io.Discard, io.Discard, env); code != 2 {
			t.Errorf("%s: exit %d for an unknown flag", command, code)
		}
		if len(built) == 0 {
			t.Errorf("%s: no flag set was built", command)
			continue
		}
		sets[command] = built[0].FlagSet
	}
	return sets
}

// referenceCommandOrder is the order commands appear in the top-level help,
// each followed by its subcommands, then version.
func referenceCommandOrder(t *testing.T) []string {
	t.Helper()
	var order []string
	for _, m := range regexp.MustCompile(`(?m)^  agent-archive (\S+)`).FindAllStringSubmatch(usage, -1) {
		order = append(order, m[1])
		var subs []string
		for command := range commandHelp {
			if strings.HasPrefix(command, m[1]+" ") {
				subs = append(subs, command)
			}
		}
		slices.Sort(subs)
		order = append(order, subs...)
	}
	order = append(order, "version")
	for command := range commandHelp {
		if !slices.Contains(order, command) {
			t.Errorf("%s has help but is missing from the top-level help", command)
		}
	}
	return order
}

// renderCLIReference renders docs/reference/cli.md from the help each
// command prints and the flags each command's flag set defines.
func renderCLIReference(t *testing.T) []byte {
	t.Helper()
	sets := commandFlagSets(t)
	var b strings.Builder
	b.WriteString(`# CLI reference

<!-- Generated from the commands' own help and flag sets by
     go test ./internal/cli -run TestCLIReferenceIsCurrent -update
     Do not edit by hand. -->

Every public command, its help as ` + "`agent-archive help COMMAND`" + ` prints it,
and the flags its parser accepts. The hidden commands ` + "`_hook`" + ` and
` + "`_collect`" + ` (what app hooks and the LaunchAgent run) are not part of the
interface and are left out. Help never activates hooks, reads credentials,
or changes state.

## Commands

` + "```text\n" + usage + "```\n")
	b.WriteString(`
## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, and help. |
| 1 | An operational failure. What a command was asked for goes to stdout; why it did not do it, or not all of it, goes to stderr. A ` + "`sync`" + ` that is paused, finds another command running, or fails for some sessions exits 1; its summary line, if it ran, stays on stdout. |
| 2 | A usage error: an unknown command or flag, a bad flag value, or an unexpected argument. It is reported in one line on stderr before the command does anything. |
| 128 + signal | ` + "`backfill`" + ` stopped at once by a second Ctrl-C (130), SIGHUP (129), or SIGTERM (143). |
`)
	for _, command := range referenceCommandOrder(t) {
		fmt.Fprintf(&b, "\n## agent-archive %s\n\n```text\n%s```\n", command, commandHelp[command])
		set, ok := sets[command]
		if !ok {
			continue
		}
		var rows []string
		set.VisitAll(func(f *flag.Flag) {
			value := "a value"
			if _, repeatable := f.Value.(*stringList); repeatable {
				value = "a value; repeatable"
			}
			if bf, isBool := f.Value.(interface{ IsBoolFlag() bool }); isBool && bf.IsBoolFlag() {
				value = "no value"
			}
			def := "—"
			if value != "no value" && f.DefValue != "" {
				def = "`" + f.DefValue + "`"
			}
			rows = append(rows, fmt.Sprintf("| `--%s` | %s | %s |", f.Name, value, def))
		})
		if len(rows) == 0 {
			b.WriteString("\nNo flags.\n")
			continue
		}
		b.WriteString("\n| Flag | Takes | Default |\n| --- | --- | --- |\n")
		b.WriteString(strings.Join(rows, "\n") + "\n")
	}
	return []byte(b.String())
}

// D-32: docs/reference/cli.md is generated from the commands' help and flag
// sets, and this fails when it is stale. Regenerate with
// `go test ./internal/cli -run TestCLIReferenceIsCurrent -update`.
func TestCLIReferenceIsCurrent(t *testing.T) {
	t.Parallel()
	golden.Check(t, cliReferencePath, renderCLIReference(t))
}
