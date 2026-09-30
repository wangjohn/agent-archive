package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// classifiedCalls says, for each file, how many times it may call a
// terminal check and why. A terminal check decides one of two kinds of thing:
//
//   - interaction (a picker, a prompt, a pager, the alternate screen, reading
//     a secret): must go through Env.interactive, which AGENT_ARCHIVE_NONINTERACTIVE
//     can turn off. Those never appear in these tables.
//   - presentation (colour, redrawing a line in place, wrapping to the
//     terminal's width) or mechanism (restoring modes a prompt changed): may
//     look at the terminal directly, since NO_COLOR and the terminal, not the
//     switch, govern them. Each is listed here with what it decides.
//
// A new call site fails TestTerminalChecksAreClassified until it is listed,
// which is when to decide which of the two it is.
type classifiedCalls map[string]map[string]int

// rawTerminalChecks are calls of Env.isTerminal.
var rawTerminalChecks = classifiedCalls{
	// The definition of interactive and its two companions: they look at the
	// terminal so that no one else has to.
	"interactive.go": {"isTerminal": 2},
	// backfill's progress line redraws in place on a terminal; that is
	// presentation. It asks nothing.
	"backfill_import.go": {"isTerminal": 1},
}

// fileDescriptorChecks are direct golang.org/x/term calls and other ways of
// asking a file descriptor whether it is a terminal.
var fileDescriptorChecks = classifiedCalls{
	// Env.isTerminal itself, the default for Env.IsTerminal.
	"cli.go": {"IsTerminal": 1},
	// styleFor: colour, redrawing, and width. Presentation, governed by
	// NO_COLOR and TERM.
	"ui.go": {"IsTerminal": 1},
	// saveTerminalState restores modes the pager or a prompt changed; it does
	// nothing unless something interactive already ran.
	"list_browse.go": {"IsTerminal": 1},
	// prompter.secret hides what is typed. Reached only from the setup
	// prompts (behind Env.interactive) and from readR2Secret, which refuses a
	// terminal that interaction is switched off for before it gets here.
	"prompt.go": {"IsTerminal": 1},
}

// promptSites are the calls of newPrompter: every place agent-archive can
// ask a question. The comment says what stops the question when interaction
// is off.
var promptSites = classifiedCalls{
	"setup.go":         {"newPrompter": 1}, // interactive setup: runSetupCommand refuses unless env.interactive(stdin) or --yes
	"setup_flags.go":   {"newPrompter": 1}, // setup --yes: only reads a secret, guarded in readR2Secret
	"uninstall.go":     {"newPrompter": 1}, // runUninstallCommand refuses unless env.interactive(stdin) or --yes
	"backfill.go":      {"newPrompter": 1}, // runBackfillCommand refuses unless env.interactive(stdin) or --yes
	"backfill_undo.go": {"newPrompter": 1}, // runBackfillUndo refuses unless env.interactive(stdin) or --yes
	"list_browse.go":   {"newPrompter": 1}, // selectArchivedSession: reached only after browseInteractive
	"inspect.go":       {"newPrompter": 2}, // list and show browsers: reached only after browseInteractive
	"show_resolve.go":  {"newPrompter": 1}, // the ambiguity picker, after browseInteractive
}

// A call of a terminal check outside the tables is a decision nobody has
// made: whether it may ask the person something an agent's shell would hang
// on. TestTerminalChecksAreClassified makes that decision explicit.
func TestTerminalChecksAreClassified(t *testing.T) {
	t.Parallel()
	found := map[string]map[string]map[string]int{"isTerminal": {}, "term": {}, "newPrompter": {}}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		note := func(kind, name string) {
			if found[kind][file] == nil {
				found[kind][file] = map[string]int{}
			}
			found[kind][file][name]++
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				switch {
				case fn.Sel.Name == "isTerminal":
					note("isTerminal", "isTerminal")
				case fn.Sel.Name == "IsTerminal" && isIdent(fn.X, "term"):
					// Not Env.IsTerminal, the injected field.
					note("term", "IsTerminal")
				case fn.Sel.Name == "IsCygwinTerminal", fn.Sel.Name == "IsTerm":
					note("term", fn.Sel.Name)
				case fn.Sel.Name == "Stat" && mentionsCharDevice(parsed):
					// A hand-rolled check: os.Stdin.Stat() and ModeCharDevice.
					note("term", "Stat")
				}
			case *ast.Ident:
				if fn.Name == "newPrompter" {
					note("newPrompter", "newPrompter")
				}
			}
			return true
		})
	}
	compare(t, "Env.isTerminal", rawTerminalChecks, found["isTerminal"], "use env.interactive(stream) for anything that asks, pages, or takes over the screen; list a presentation-only check in rawTerminalChecks")
	compare(t, "file descriptor terminal check", fileDescriptorChecks, found["term"], "use env.interactive(stream); list a presentation or mechanism check in fileDescriptorChecks")
	compare(t, "newPrompter", promptSites, found["newPrompter"], "a new question must be unreachable when interaction is off: gate it with env.interactive, then list it in promptSites")
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

// mentionsCharDevice reports whether a file tests os.ModeCharDevice, the
// hand-rolled way to ask whether stdin is a terminal.
func mentionsCharDevice(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "ModeCharDevice" {
			found = true
		}
		return !found
	})
	return found
}

func compare(t *testing.T, what string, want classifiedCalls, got map[string]map[string]int, advice string) {
	t.Helper()
	var files []string
	seen := map[string]bool{}
	for file := range want {
		files, seen[file] = append(files, file), true
	}
	for file := range got {
		if !seen[file] {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	for _, file := range files {
		for name := range union(want[file], got[file]) {
			if want[file][name] != got[file][name] {
				t.Errorf("%s: %d %s call(s) of %s, but %d are classified. %s", file, got[file][name], what, name, want[file][name], advice)
			}
		}
	}
}

func union(a, b map[string]int) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
