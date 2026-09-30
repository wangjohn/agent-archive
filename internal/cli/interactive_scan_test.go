package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// classifiedCalls says, for each file, how many times it may use a
// terminal check (or read standard input) and why. A terminal check decides
// one of two kinds of thing:
//
//   - interaction (a picker, a prompt, a pager, the alternate screen, reading
//     a secret): must go through Env.interactive, which AGENT_ARCHIVE_NONINTERACTIVE
//     can turn off. Those never appear in these tables.
//   - presentation (colour, redrawing a line in place, wrapping to the
//     terminal's width) or mechanism (restoring modes a prompt changed): may
//     look at the terminal directly, since NO_COLOR and the terminal, not the
//     switch, govern them. Each is listed here with what it decides.
//
// A new use fails TestTerminalChecksAreClassified until it is listed, which
// is when to decide which of the two it is.
//
// The scan counts every mention, not only calls, so a method value
// (check := env.isTerminal) or a renamed import (import t "golang.org/x/term")
// cannot slip past it. It is syntactic, so it cannot follow a terminal check
// made through a value passed in from another package, or a stream read
// through a helper that is itself listed; that is what the classification
// comments and code review are for.
type classifiedCalls map[string]map[string]int

// rawTerminalChecks are mentions of Env.isTerminal.
var rawTerminalChecks = classifiedCalls{
	// The definition of interactive and its two companions: they look at the
	// terminal so that no one else has to.
	"interactive.go": {"isTerminal": 2},
	// backfill's progress line redraws in place on a terminal; that is
	// presentation. It asks nothing.
	"backfill_import.go": {"isTerminal": 1},
	// stats --html refuses to write a web page onto a terminal (it asks for
	// --output instead). That is a refusal, not a prompt, and it holds inside
	// an agent too, where a page on the pty would land in the agent's
	// context, so it asks the terminal itself rather than Env.interactive.
	"stats.go": {"isTerminal": 1},
}

// injectedTerminalChecks are mentions of the Env.IsTerminal field itself,
// which Env.isTerminal wraps.
var injectedTerminalChecks = classifiedCalls{
	// Env.isTerminal: the nil check and the call.
	"cli.go": {"IsTerminal": 2},
}

// terminalPackageUses are uses of packages that ask a file descriptor about
// its terminal (golang.org/x/term and its kin), by whatever name they are
// imported under.
var terminalPackageUses = classifiedCalls{
	// Env.isTerminal itself, the default for Env.IsTerminal.
	"cli.go": {"term.IsTerminal": 1},
	// styleFor: colour, redrawing, and width. Presentation, governed by
	// NO_COLOR and TERM.
	"ui.go": {"term.IsTerminal": 1, "term.GetSize": 1},
	// saveTerminalState restores modes the pager or a prompt changed; it does
	// nothing unless something interactive already ran.
	"list_browse.go": {"term.IsTerminal": 1, "term.GetState": 1, "term.Restore": 1},
	// prompter.secret hides what is typed. Reached only from the setup
	// prompts (behind Env.interactive) and from readR2Secret, which refuses a
	// terminal that interaction is switched off for before it gets here.
	"prompt.go": {"term.IsTerminal": 1, "term.GetState": 1, "term.Restore": 2, "term.ReadPassword": 1},
}

// promptSites are the mentions of newPrompter (and of a prompter built by
// hand): every place agent-archive can ask a question. The comment says what
// stops the question when interaction is off.
var promptSites = classifiedCalls{
	"prompt.go":        {"newPrompter": 1, "prompter{}": 1}, // the definition
	"setup.go":         {"newPrompter": 1},                  // interactive setup: runSetupCommand refuses unless env.interactive(stdin) or --yes
	"setup_flags.go":   {"newPrompter": 1, "prompter{}": 1}, // setup --yes: only reads a secret, guarded in readR2Secret; the literal has no input, it only prints
	"uninstall.go":     {"newPrompter": 1},                  // runUninstallCommand refuses unless env.interactive(stdin) or --yes
	"backfill.go":      {"newPrompter": 1},                  // runBackfillCommand refuses unless env.interactive(stdin) or --yes
	"backfill_undo.go": {"newPrompter": 1},                  // runBackfillUndo refuses unless env.interactive(stdin) or --yes
	"list_browse.go":   {"newPrompter": 1},                  // selectArchivedSession: reached only after browseInteractive
	"inspect.go":       {"newPrompter": 2},                  // list and show browsers: reached only after browseInteractive
	"show_resolve.go":  {"newPrompter": 1},                  // the ambiguity picker, after browseInteractive
}

// inputReads are the ways a command reads a stream it was handed, other than
// through a prompter. A read of standard input that waits for a person must
// be refused when interaction is off; a read of a file need not be.
var inputReads = classifiedCalls{
	// The prompter's own line reader: every prompt (see promptSites).
	"prompt.go": {"bufio.NewReader": 1},
	// The hook payload the agent writes and closes; never a person.
	"hook_command.go": {"json.NewDecoder": 1},
	// purge apply's typed digest: refused in runPurgeApply when the switch is
	// on and --yes is not given.
	"purge.go": {"bufio.NewReader": 1},
	// Files, not standard input.
	"setup_aws.go": {"bufio.NewScanner": 1},
	"feedback.go":  {"io.ReadAll": 1},
	// stats --prices: a file the person names, read in full, bounded.
	"stats.go": {"io.ReadAll": 1},
}

// terminalImports are packages whose only use here is to ask about, or
// change, a terminal.
var terminalImports = map[string]bool{
	"golang.org/x/term":          true,
	"golang.org/x/sys/unix":      true,
	"github.com/mattn/go-isatty": true,
}

// readers are the selectors that read a stream to its end or line by line.
var readers = map[string]bool{
	"bufio.NewReader": true, "bufio.NewReaderSize": true, "bufio.NewScanner": true,
	"json.NewDecoder": true, "io.ReadAll": true, "io.ReadFull": true, "io.Copy": true,
	"fmt.Fscan": true, "fmt.Fscanf": true, "fmt.Fscanln": true, "ioutil.ReadAll": true,
}

// terminalUses counts, by kind and name, everything in file that asks about
// a terminal, builds a prompt, or reads a stream.
func terminalUses(t *testing.T, file *ast.File) map[string]map[string]int {
	t.Helper()
	found := map[string]map[string]int{}
	note := func(kind, name string) {
		if found[kind] == nil {
			found[kind] = map[string]int{}
		}
		found[kind][name]++
	}
	terminalPackages := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !terminalImports[importPath] {
			continue
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		//lint:ignore LV1001 the import name as written in source, not a value this package defines
		if local == "." || local == "_" {
			note("term", "import "+local+" "+importPath)
			continue
		}
		terminalPackages[local] = true
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			x, _ := n.X.(*ast.Ident)
			switch {
			case n.Sel.Name == "isTerminal":
				note("isTerminal", "isTerminal")
			case x != nil && terminalPackages[x.Name]:
				note("term", x.Name+"."+n.Sel.Name)
			case n.Sel.Name == "IsTerminal":
				note("field", "IsTerminal")
			case n.Sel.Name == "ModeCharDevice":
				// A hand-rolled check: os.Stdin.Stat() and ModeCharDevice.
				note("term", "ModeCharDevice")
			case x != nil && readers[x.Name+"."+n.Sel.Name]:
				note("read", x.Name+"."+n.Sel.Name)
			case x != nil && x.Name == "os" && n.Sel.Name == "Stdin":
				note("read", "os.Stdin")
			}
		case *ast.Ident:
			// Every mention: a call, a method value, an alias.
			if n.Name == "newPrompter" {
				note("newPrompter", "newPrompter")
			}
		case *ast.CompositeLit:
			// A prompter built by hand skips newPrompter.
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "prompter" {
				note("newPrompter", "prompter{}")
			}
		}
		return true
	})
	return found
}

// A use outside the tables is a decision nobody has made: whether it may ask
// the person something an agent's shell would hang on.
// TestTerminalChecksAreClassified makes that decision explicit.
func TestTerminalChecksAreClassified(t *testing.T) {
	t.Parallel()
	found := map[string]map[string]map[string]int{"isTerminal": {}, "field": {}, "term": {}, "newPrompter": {}, "read": {}}
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
		for kind, names := range terminalUses(t, parsed) {
			found[kind][file] = names
		}
	}
	compare(t, "Env.isTerminal", rawTerminalChecks, found["isTerminal"], "use env.interactive(stream) for anything that asks, pages, or takes over the screen; list a presentation-only check in rawTerminalChecks")
	compare(t, "Env.IsTerminal field", injectedTerminalChecks, found["field"], "use env.interactive(stream); Env.isTerminal is the only reader of the field")
	compare(t, "terminal package", terminalPackageUses, found["term"], "use env.interactive(stream); list a presentation or mechanism use in terminalPackageUses")
	compare(t, "newPrompter", promptSites, found["newPrompter"], "a new question must be unreachable when interaction is off: gate it with env.interactive, then list it in promptSites")
	compare(t, "stream read", inputReads, found["read"], "a read of standard input that waits for a person must be refused when interaction is off (see runPurgeApply); list a read that never waits for one in inputReads")
}

// The scan must notice the ways a check could be hidden from a plain search
// for calls: a method value, a renamed import, an alias of newPrompter, a
// prompter built by hand, a hand-rolled character device test.
func TestTerminalScanNoticesHiddenChecks(t *testing.T) {
	t.Parallel()
	source := `package cli

import (
	"bufio"
	"os"
	t "golang.org/x/term"
)

func a(e Env) { check := e.isTerminal; _ = check }
func b(e Env) { _ = t.IsTerminal; _ = t.ReadPassword }
func c(in io.Reader) { _ = bufio.NewReader(in); p := newPrompter; _ = p; _ = &prompter{} }
func d() { info, _ := os.Stdin.Stat(); _ = info.Mode() & os.ModeCharDevice }
func e2(e Env) { _ = e.IsTerminal }
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"isTerminal":  {"isTerminal": 1},
		"term":        {"t.IsTerminal": 1, "t.ReadPassword": 1, "ModeCharDevice": 1},
		"read":        {"bufio.NewReader": 1, "os.Stdin": 1},
		"newPrompter": {"newPrompter": 1, "prompter{}": 1},
		"field":       {"IsTerminal": 1},
	}
	got := terminalUses(t, parsed)
	for kind, names := range want {
		for name, count := range names {
			if got[kind][name] != count {
				t.Errorf("%s %s: noticed %d, want %d", kind, name, got[kind][name], count)
			}
		}
	}
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
				t.Errorf("%s: %d %s use(s) of %s, but %d are classified. %s", file, got[file][name], what, name, want[file][name], advice)
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
