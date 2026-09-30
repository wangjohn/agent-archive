package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statshtml"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// statsHTMLFlags are the flags that turn `stats` into a web page: --html, and
// where the page goes. They follow handoff's --output and --force: the file
// is written with mode 0600 and an existing file is never replaced without
// --force.
type statsHTMLFlags struct {
	html         bool
	output       string
	force        bool
	includeNames bool
}

func addStatsHTMLFlags(fs *commandFlags) *statsHTMLFlags {
	f := &statsHTMLFlags{}
	fs.BoolVar(&f.html, "html", false, "write one self-contained HTML page (inline styles and SVG, no script, no external requests) to stdout, or to --output")
	fs.StringVar(&f.output, "output", "", "with --html, write the page to this file (mode 0600) instead of stdout")
	fs.BoolVar(&f.force, "force", false, "with --output, replace an existing file")
	fs.BoolVar(&f.includeNames, "include-project-names", false, "with --html, name the real projects; by default the page says project A, B, ... so it can be shared")
	return f
}

// validate reports a usage error for flags that do not fit together, before
// any storage is touched: --html is not --json, its options need it, the page
// is not dumped onto a terminal, and a file that would be replaced needs
// --force.
func (f *statsHTMLFlags) validate(fs *commandFlags, jsonOut, stdoutIsTerminal bool) int {
	switch {
	case f.html && jsonOut:
		return fs.usageError("--html and --json cannot be combined; choose one")
	case !f.html && f.output != "":
		return fs.usageError("--output applies only to --html")
	case !f.html && f.includeNames:
		return fs.usageError("--include-project-names applies only to --html")
	case f.force && f.output == "":
		return fs.usageError("--force applies only to --output")
	case f.html && f.output == "" && stdoutIsTerminal:
		return fs.usageError("--html writes a web page: give --output FILE (for example --output stats.html), or redirect standard output")
	}
	if f.output == "" {
		return 0
	}
	if err := checkStatsHTMLTarget(f.output, f.force); err != nil {
		return fs.usageError("--output: %v", err)
	}
	return 0
}

// checkStatsHTMLTarget fails early, before the archive is read, when the page
// could not be written: the file exists and may not be replaced, or its
// directory is missing.
func checkStatsHTMLTarget(path string, force bool) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("%s is a directory", path)
	case err == nil && !force:
		return fmt.Errorf("%s already exists; pass --force to replace it", path)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("the folder for %s does not exist or cannot be read: %w", path, err)
	}
	if !dir.IsDir() {
		return fmt.Errorf("%s is not a folder", filepath.Dir(path))
	}
	return nil
}

// toStdout is whether the page goes to standard output, which then carries
// nothing but the page.
func (f *statsHTMLFlags) toStdout() bool { return f.html && f.output == "" }

// write renders the page and delivers it: to stdout, or to the --output file
// (with a note on stderr, as handoff does). The page is rendered before
// anything is created, so a failure leaves no file behind.
func (f *statsHTMLFlags) write(stdout, stderr io.Writer, computed stats.Stats, filters statsFilters, now time.Time, emptyMessage string) int {
	page, err := statshtml.Render(computed, statshtml.Options{
		GeneratedAt:         now,
		IncludeProjectNames: f.includeNames,
		Filters:             statshtml.Filters{Harness: filters.Harness, Model: filters.Model, Origin: filters.Origin},
		EmptyMessage:        emptyMessage,
	})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	if f.output == "" {
		terminal.Print(stdout, string(page))
		return 0
	}
	if err := writeHandoffOutput(f.output, page, f.force); err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	terminal.Printf(stderr, "stats: wrote %s (%d bytes); open it in a browser\n", f.output, len(page))
	return 0
}
