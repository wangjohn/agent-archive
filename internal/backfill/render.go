package backfill

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// harnessOrder is the order apps appear in, in columns and lists.
var harnessOrder = []string{"claude", "codex", "cursor"}

var harnessNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor"}

// ProjectSummary is one project row of the plan: the sessions it imports.
type ProjectSummary struct {
	Root string
	Kind ProjectKind
	// Included is set when the project is already configured and included.
	Included  bool
	Exists    bool
	Sessions  map[string]int
	Subagents int
	// Bytes counts the sessions' and their subagents' transcripts.
	Bytes      int64
	FirstStart time.Time
	LastStart  time.Time
	// KeptOut are the folders inside a project the plan adds that the
	// import adds as excluded projects, so the new project does not capture
	// them (see nested.go). KeptOutUnchecked are those among them kept out
	// whole without being looked in, because macOS protects them.
	// NestedComplete is false when not every other folder inside could be
	// looked in.
	KeptOut          []string
	KeptOutUnchecked []string
	NestedComplete   bool
}

// CapturesSubfolders reports whether adding the project makes hooks capture
// new sessions in folders under it that no nearer project owns.
func (s ProjectSummary) CapturesSubfolders() bool {
	return !s.Included && capturesSubfolders(s.Kind)
}

// Total is the number of sessions the project imports.
func (s ProjectSummary) Total() int {
	total := 0
	for _, n := range s.Sessions {
		total += n
	}
	return total
}

// Imported returns the candidates the plan imports.
func (p Plan) Imported() []Candidate {
	var out []Candidate
	for _, c := range p.Candidates {
		if c.Skip == "" {
			out = append(out, c)
		}
	}
	return out
}

// Found is how many sessions discovery found, imported or not, Cursor
// database chats included.
func (p Plan) Found() int {
	return len(p.Candidates)
}

// Projects groups the imported sessions by project, sorted by total and then
// by path.
func (p Plan) Projects() []ProjectSummary {
	byRoot := map[string]*ProjectSummary{}
	var order []*ProjectSummary
	for _, c := range p.Imported() {
		s, ok := byRoot[c.ProjectRoot]
		if !ok {
			s = &ProjectSummary{Root: c.ProjectRoot, Kind: c.ProjectKind, Included: c.ProjectIncluded, Exists: c.ProjectExists, Sessions: map[string]int{}}
			byRoot[c.ProjectRoot] = s
			order = append(order, s)
		}
		s.Sessions[c.Harness]++
		s.Subagents += len(c.Subagents)
		s.Bytes += c.Bytes
		for _, sub := range c.Subagents {
			s.Bytes += sub.Bytes
		}
		if !c.StartedAt.IsZero() {
			if s.FirstStart.IsZero() || c.StartedAt.Before(s.FirstStart) {
				s.FirstStart = c.StartedAt
			}
			if c.StartedAt.After(s.LastStart) {
				s.LastStart = c.StartedAt
			}
		}
	}
	out := make([]ProjectSummary, len(order))
	for i, s := range order {
		out[i] = *s
		out[i].NestedComplete = true
		if nested, ok := p.nested[s.Root]; ok {
			out[i].KeptOut, out[i].KeptOutUnchecked, out[i].NestedComplete = nested.KeptOut, nested.Unchecked, nested.Complete
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].Total(), out[j].Total(); a != b {
			return a > b
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// Skipped counts the sessions not imported, by reason. Zero counts are left
// out.
func (p Plan) Skipped() map[SkipReason]int {
	counts := map[SkipReason]int{}
	for _, c := range p.Candidates {
		if c.Skip != "" {
			counts[c.Skip]++
		}
	}
	return counts
}

// SubagentsSkipped counts the subagent transcripts of imported sessions that
// are left out because they are too large, the filter refuses them, or the
// collector would not register them.
func (p Plan) SubagentsSkipped() int {
	n := 0
	for _, c := range p.Candidates {
		if c.Skip == "" {
			n += c.SubagentsSkipped
		}
	}
	return n
}

// AppsWithoutHooks lists the apps the plan imports sessions for that have no
// hooks installed, so their new sessions are not captured.
func (p Plan) AppsWithoutHooks() []string {
	out := []string{}
	if len(p.Harnesses) == 0 {
		// An older configuration without an app list accepts every app.
		return out
	}
	importing := map[string]bool{}
	for _, c := range p.Imported() {
		importing[c.Harness] = true
	}
	for _, h := range harnessOrder {
		if importing[h] && !slices.Contains(p.Harnesses, h) {
			out = append(out, h)
		}
	}
	return out
}

// ExpiresOn is the local date retention deletes sessions imported now, or
// false when retention is off.
func (p Plan) ExpiresOn() (string, bool) {
	if p.RetentionDays <= 0 {
		return "", false
	}
	return p.GeneratedAt.AddDate(0, 0, p.RetentionDays).Format(dateLayout), true
}

// SearchLine is the progress line printed while discovery runs.
func SearchLine(f Filters) string {
	var names []string
	for _, h := range harnessOrder {
		if harnessMatches(f.Harnesses, h) {
			names = append(names, harnessNames[h])
		}
	}
	return fmt.Sprintf("Looking for %s sessions on this Mac…", joinAnd(names))
}

// RenderText writes the plan as the spec's default run shows it, without the
// progress lines or the final prompt. It prints project roots and counts
// only: never a transcript path, a native ID, or content.
func RenderText(w io.Writer, p Plan) {
	projects := p.Projects()
	if len(projects) == 0 {
		terminal.Println(w, "Nothing to import.")
		renderSkipped(w, p)
		return
	}
	if p.Filters.Active() {
		terminal.Printf(w, "Backfill imports sessions matching %s\ninto %s. Nothing has been uploaded yet.\n", p.filterFlags(), p.destination())
	} else {
		terminal.Printf(w, "Backfill imports every session found on this Mac into\n%s. Nothing has been uploaded yet.\n", p.destination())
	}
	terminal.Println(w)

	var repos, others []ProjectSummary
	for _, s := range projects {
		if s.Kind == ProjectKindRepository {
			repos = append(repos, s)
		} else {
			others = append(others, s)
		}
	}
	width := 38
	for _, s := range projects {
		width = max(width, utf8.RuneCountInString(p.rowLabel(s))+2)
	}
	terminal.Printf(w, "%-*s%6s  %5s  %6s  %5s\n", width, "PROJECT", "CLAUDE", "CODEX", "CURSOR", "TOTAL")
	for _, s := range repos {
		p.renderRow(w, width, s)
	}
	if len(others) > 0 {
		if len(repos) > 0 {
			terminal.Println(w)
		}
		terminal.Println(w, "Not a repository:")
		for _, s := range others {
			p.renderRow(w, width, s)
		}
	}
	terminal.Println(w)

	sessions, subagents := 0, 0
	var bytes int64
	var first, last time.Time
	for _, s := range projects {
		sessions += s.Total()
		subagents += s.Subagents
		bytes += s.Bytes
		if !s.FirstStart.IsZero() && (first.IsZero() || s.FirstStart.Before(first)) {
			first = s.FirstStart
		}
		if s.LastStart.After(last) {
			last = s.LastStart
		}
	}
	total := "Total: " + CountNoun(sessions, "session")
	if subagents > 0 {
		total += fmt.Sprintf(" (plus %s)", CountNoun(subagents, "subagent transcript"))
	}
	terminal.Printf(w, "%s, %s,\n", total, FormatSize(bytes))
	loc := p.GeneratedAt.Location()
	firstDay, lastDay := first.In(loc).Format(dateLayout), last.In(loc).Format(dateLayout)
	if firstDay == lastDay {
		terminal.Printf(w, "       started %s.\n", firstDay)
	} else {
		terminal.Printf(w, "       started %s to %s.\n", firstDay, lastDay)
	}

	renderSkipped(w, p)
	terminal.Println(w)
	terminal.Println(w, "If you continue:")
	added := 0
	for _, s := range projects {
		if !s.Included {
			added++
		}
	}
	var hooked []string
	for _, h := range harnessOrder {
		if slices.Contains(p.Harnesses, h) {
			hooked = append(hooked, harnessNames[h])
		}
	}
	missing := p.AppsWithoutHooks()
	var missingNames []string
	for _, h := range missing {
		missingNames = append(missingNames, harnessNames[h])
	}
	needSetup := ""
	if len(missing) == 1 {
		needSetup = fmt.Sprintf("New %s sessions need that app added in setup.", missingNames[0])
	} else if len(missing) > 1 {
		needSetup = fmt.Sprintf("New %s sessions need those apps added in setup.", joinAnd(missingNames))
	}
	switch {
	case added > 0:
		verb := "projects are"
		if added == 1 {
			verb = "project is"
		}
		captured := "new sessions in them are captured."
		if added == 1 {
			captured = "new sessions in it are captured."
		}
		if len(hooked) > 0 {
			captured = strings.Replace(captured, "new sessions", "new "+joinAnd(hooked)+" sessions", 1)
		}
		terminal.Printf(w, "  • %d %s added, and %s\n", added, verb, captured)
		if needSetup != "" {
			terminal.Printf(w, "    %s\n", needSetup)
		}
	case needSetup != "":
		terminal.Printf(w, "  • %s\n", needSetup)
	}
	if expires, ok := p.ExpiresOn(); ok {
		terminal.Printf(w, "  • Retention is %d days, so these sessions are deleted on %s.\n    Choose `edit` to keep them longer.\n", p.RetentionDays, expires)
	} else {
		terminal.Println(w, "  • Retention is off, so these sessions are kept until you delete them.")
	}
	terminal.Println(w, "  • Undo any time with `agent-archive backfill undo`.")
}

func (p Plan) renderRow(w io.Writer, width int, s ProjectSummary) {
	cells := make([]string, 0, 4)
	for _, h := range harnessOrder {
		cells = append(cells, countCell(s.Sessions[h]))
	}
	status := "will be added"
	if s.Included {
		status = "already included"
	}
	terminal.Printf(w, "%-*s%6s  %5s  %6s  %5d  %s\n", width, p.rowLabel(s), cells[0], cells[1], cells[2], s.Total(), status)
	switch s.Kind {
	case ProjectKindScratch:
		if p.isCodexWorkspaces(s.Root) {
			terminal.Println(w, "  Chats in the workspaces Codex creates for them. New ones will be captured too.")
		} else {
			terminal.Println(w, "  Chats started without a folder. New ones will be captured too.")
		}
	case ProjectKindHome:
		if s.Included {
			break
		}
		terminal.Println(w, "  Every future session under your home folder that isn't in a nearer project")
		terminal.Println(w, "  will be captured too.")
	case ProjectKindTemporary, ProjectKindDirectory:
		if s.CapturesSubfolders() {
			p.renderSubfolderNote(w, s)
		}
	case ProjectKindRepository:
		// No note under the row.
	}
}

// maxKeptOutShown is how many kept-out folders a row lists by name.
const maxKeptOutShown = 5

// renderSubfolderNote says what adding a plain folder captures: every
// future session in a folder under it that no nearer project owns, except
// the folders the import keeps out, which it names.
func (p Plan) renderSubfolderNote(w io.Writer, s ProjectSummary) {
	terminal.Println(w, "  Every future session in a folder under it that isn't in a nearer project")
	terminal.Println(w, "  will be captured too.")
	if n := len(s.KeptOut); n > 0 {
		switch {
		case n == 1 && len(s.KeptOutUnchecked) > 0:
			terminal.Println(w, "  One folder inside it, which macOS protects, is added as an excluded")
			terminal.Println(w, "  project so it stays out of capture; setup can include it:")
		case n == 1:
			terminal.Println(w, "  One folder inside it, a repository or app folder, is added as an excluded")
			terminal.Println(w, "  project so it stays out of capture; setup can include it:")
		case len(s.KeptOutUnchecked) > 0:
			terminal.Printf(w, "  %d folders inside it (repositories, app folders, or folders macOS\n", n)
			terminal.Println(w, "  protects) are added as excluded projects so they stay out of capture;")
			terminal.Println(w, "  setup can include them:")
		default:
			terminal.Printf(w, "  %d folders inside it, repositories or app folders, are added as excluded\n", n)
			terminal.Println(w, "  projects so they stay out of capture; setup can include them:")
		}
		for i, folder := range s.KeptOut {
			if i == maxKeptOutShown {
				terminal.Printf(w, "    and %d more\n", n-maxKeptOutShown)
				break
			}
			terminal.Printf(w, "    %s\n", p.display(folder))
		}
	}
	if len(s.KeptOutUnchecked) > 0 {
		names := make([]string, 0, len(s.KeptOutUnchecked))
		for _, folder := range s.KeptOutUnchecked {
			names = append(names, p.display(folder))
		}
		they := "it was"
		if len(names) > 1 {
			they = "they were"
		}
		terminal.Printf(w, "  macOS asks before an app reads %s,\n", joinAnd(names))
		terminal.Printf(w, "  so %s not looked in and %s out of capture whole.\n", they, stayStays(len(names)))
	}
	if !s.NestedComplete {
		terminal.Println(w, "  Not every folder inside it could be checked for repositories, so a")
		terminal.Println(w, "  repository in it that was not found is captured too. To keep capture out")
		terminal.Println(w, "  of this folder, exclude it later in agent-archive setup (Change apps and")
		terminal.Println(w, "  projects), or import only the projects you want with --project.")
	}
}

func (p Plan) rowLabel(s ProjectSummary) string {
	label := p.display(s.Root)
	if s.Kind == ProjectKindScratch {
		label = "Claude desktop scratch chats"
		if p.isCodexWorkspaces(s.Root) {
			label = "Codex desktop workspaces"
		}
	}
	if !s.Exists {
		label += " (folder no longer exists)"
	}
	return label
}

// isCodexWorkspaces reports whether root is Codex desktop's workspace
// folder, ~/Documents/Codex.
func (p Plan) isCodexWorkspaces(root string) bool {
	return p.display(root) == filepath.Join("~", "Documents", "Codex")
}

func countCell(n int) string {
	if n == 0 {
		return "–"
	}
	return strconv.Itoa(n)
}

// skipLabels describe each reason, after its count.
var skipLabels = map[SkipReason]string{
	SkipAlreadyArchived:       "already in the archive",
	SkipDuplicateSession:      "same session found more than once",
	SkipRegisteredNotAdmitted: "registered earlier, not accepted by the current setup",
	SkipRemovedByUndo:         "removed earlier by undo",
	SkipRemovedByRetention:    "removed earlier by retention",
	SkipFilteredOut:           "not matching the filters",
	SkipExcludedProject:       "in projects excluded in setup",
	SkipHomeDirectory:         "run from the home folder",
	SkipAboveHome:             "run from / or /Users, above the home folder",
	SkipTemporaryDirectory:    "run from temporary directories",
	SkipProjectUnknown:        "%s whose project could not be determined",
	SkipWorktreeUnresolved:    "from worktrees that no longer exist",
	SkipIdentityMismatch:      "with IDs that do not match the file",
	SkipEmpty:                 "with no conversation",
	SkipUnsafeFormat:          "in a format the archive cannot read safely",
	SkipTooLarge:              "over the size limit",
	SkipStartUnknown:          "%s whose start time could not be determined",
	SkipStartInFuture:         "starting in the future (check the clock)",
}

// skipOverrides are the flags or steps that import a skipped session anyway.
var skipOverrides = map[SkipReason]string{
	SkipRemovedByUndo:      "add --include-removed",
	SkipRemovedByRetention: "add --include-removed",
	SkipExcludedProject:    "change in setup",
	SkipHomeDirectory:      "add --include-home",
	SkipTemporaryDirectory: "add --include-temp",
}

func renderSkipped(w io.Writer, p Plan) {
	counts := p.Skipped()
	subagentsSkipped := p.SubagentsSkipped()
	databaseUnchecked := !p.CursorDatabaseChecked && harnessMatches(p.Filters.Harnesses, "cursor")
	if len(counts) == 0 && subagentsSkipped == 0 && p.CursorSubagentsNotImported == 0 && p.UnreadableFolders == 0 && len(p.UnreadableStores) == 0 && !databaseUnchecked {
		return
	}
	nouns := map[SkipReason]map[string]bool{}
	for _, c := range p.Candidates {
		if c.Skip != "" {
			if nouns[c.Skip] == nil {
				nouns[c.Skip] = map[string]bool{}
			}
			nouns[c.Skip][c.Harness] = true
		}
	}
	type line struct {
		n        int
		label    string
		override string
	}
	var lines []line
	width := 0
	for _, reason := range skipOrder {
		n := counts[reason]
		if n == 0 {
			continue
		}
		label := skipLabels[reason]
		if strings.Contains(label, "%s") {
			label = fmt.Sprintf(label, sessionNoun(nouns[reason], n))
		}
		lines = append(lines, line{n, label, skipOverrides[reason]})
		if skipOverrides[reason] != "" {
			width = max(width, utf8.RuneCountInString(label))
		}
	}
	if subagentsSkipped > 0 {
		label := "subagent transcripts that can't be imported"
		if subagentsSkipped == 1 {
			label = "subagent transcript that can't be imported"
		}
		lines = append(lines, line{subagentsSkipped, label, ""})
	}
	if n := p.CursorSubagentsNotImported; n > 0 {
		label := "Cursor subagent chats are not imported yet"
		if n == 1 {
			label = "Cursor subagent chat is not imported yet"
		}
		lines = append(lines, line{n, label, ""})
	}
	if n := p.UnreadableFolders; n > 0 {
		label := "folders in the app stores could not be read"
		if n == 1 {
			label = "folder in the app stores could not be read"
		}
		lines = append(lines, line{n, label, ""})
	}
	width = max(width+2, 42)
	terminal.Println(w)
	terminal.Println(w, "Not imported:")
	for _, l := range lines {
		if l.override == "" {
			terminal.Printf(w, "%4d  %s\n", l.n, l.label)
		} else {
			terminal.Printf(w, "%4d  %-*s%s\n", l.n, width, l.label, l.override)
		}
	}
	for _, h := range p.UnreadableStores {
		if h == "codex" && p.codexArchivedOnly {
			terminal.Println(w, "      Codex's archived session folder could not be read (check permissions);")
			terminal.Println(w, "      none of its archived sessions are included.")
			continue
		}
		terminal.Printf(w, "      %s's session folder could not be read (check permissions);\n", harnessNames[h])
		terminal.Println(w, "      none of its sessions are included.")
	}
	if databaseUnchecked {
		terminal.Println(w, "      Cursor chats stored only in Cursor's database were not checked:")
		terminal.Printf(w, "      %s.\n", uncheckedCauses[p.CursorDatabaseUnchecked])
	}
}

// uncheckedCauses explain why Cursor's database was not checked. "" is a
// plan made without a database reader.
var uncheckedCauses = map[CursorUncheckedReason]string{
	"":                                   "the database was not read",
	CursorUncheckedLocked:                "it has an unfinished write; open and close Cursor, then try again",
	CursorUncheckedUnreadable:            "the database could not be read safely",
	CursorUncheckedUnknownFormat:         "it is in a format this version does not know",
	CursorUncheckedChangedDuringRead:     "Cursor changed it while it was read; try again",
	CursorUncheckedTranscriptsUnreadable: "Cursor's transcript folders could not all be read, so chats with transcripts can't be told apart",
}

// sessionNoun names n sessions from the given apps: "Cursor chat" when they
// are all Cursor's, and so on.
func sessionNoun(apps map[string]bool, n int) string {
	noun := "session"
	if len(apps) == 1 {
		for h := range apps {
			if h == "cursor" {
				noun = "Cursor chat"
			} else {
				noun = harnessNames[h] + " session"
			}
		}
	}
	if n != 1 {
		noun += "s"
	}
	return noun
}

// display shows a path with the home directory as ~.
func (p Plan) display(path string) string {
	for _, home := range p.homeForms() {
		if path == home {
			return "~"
		}
		if strings.HasPrefix(path, home+string(filepath.Separator)) {
			return "~" + path[len(home):]
		}
	}
	return path
}

func (p Plan) homeForms() []string {
	forms := []string{}
	if p.Home != "" {
		forms = append(forms, filepath.Clean(p.Home))
	}
	if p.resolvedHome != "" && p.resolvedHome != filepath.Clean(p.Home) {
		forms = append(forms, p.resolvedHome)
	}
	return forms
}

func (p Plan) destination() string {
	parts := []string{p.Destination.Provider, p.Destination.Bucket}
	if prefix := p.Destination.Prefix; prefix != "" {
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		parts = append(parts, prefix)
	}
	return strings.Join(parts, " / ")
}

// filterFlags lists the filters that narrow the run, as the flags that set
// them.
func (p Plan) filterFlags() string {
	var flags []string
	for _, h := range p.Filters.Harnesses {
		flags = append(flags, "--harness "+archive.CanonicalHarness(h))
	}
	for _, dir := range p.Filters.Projects {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		flags = append(flags, "--project "+p.display(dir))
	}
	if p.Filters.Since != "" {
		flags = append(flags, "--since "+p.Filters.Since)
	}
	if p.Filters.Until != "" {
		flags = append(flags, "--until "+p.Filters.Until)
	}
	return strings.Join(flags, " ")
}

// CountNoun is n and noun, pluralized with "s" unless n is 1: "1 session",
// "3 projects". The CLI uses it too, so every message counts alike.
func CountNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

// FormatSize shows a byte count in decimal units, as Finder does.
func FormatSize(n int64) string {
	switch {
	case n < 1000:
		return CountNoun(int(n), "byte")
	case n < 1000*1000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%d MB", (n+500*1000)/(1000*1000))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

// planJSON is `backfill --dry-run --json`.
type planJSON struct {
	Destination      Destination        `json:"destination"`
	Filters          filtersJSON        `json:"filters"`
	Projects         []projectJSON      `json:"projects"`
	Skipped          map[SkipReason]int `json:"skipped"`
	AppsWithoutHooks []string           `json:"apps_without_hooks"`
	RetentionDays    int                `json:"retention_days"`
	// ExpiresOn is empty when retention is off.
	ExpiresOn string `json:"expires_on"`
	// StorageChecked is always false. This JSON is printed only by a dry
	// run, which writes nothing, locally or remotely, and checking storage
	// writes a test object; only an import checks it, before its prompt.
	// The key is kept so scripts that read it keep working.
	StorageChecked        bool `json:"storage_checked"`
	CursorDatabaseChecked bool `json:"cursor_database_checked"`
	// CursorDatabaseUncheckedReason is set when the database was not
	// checked: locked, unreadable, unknown_format, changed_during_read, or
	// transcripts_unreadable (see CursorUncheckedReason).
	CursorDatabaseUncheckedReason CursorUncheckedReason `json:"cursor_database_unchecked_reason,omitempty"`
	// CursorDatabaseNewerFormat counts the database rows read although
	// their format version is newer than this release knows.
	CursorDatabaseNewerFormat int `json:"cursor_database_newer_format"`
	// CursorSubagentsNotImported counts the subagent chats of the Cursor
	// chats imported; they are not imported yet.
	CursorSubagentsNotImported int `json:"cursor_subagents_not_imported"`
	SubagentsSkipped           int `json:"subagents_skipped"`
	UnreadableFolders          int `json:"unreadable_folders"`
	// UnreadableStores are the apps whose whole session store could not
	// be read.
	UnreadableStores []string `json:"unreadable_stores"`
}

type filtersJSON struct {
	Harnesses      []string `json:"harnesses"`
	Projects       []string `json:"projects"`
	Since          string   `json:"since,omitempty"`
	Until          string   `json:"until,omitempty"`
	IncludeHome    bool     `json:"include_home"`
	IncludeTemp    bool     `json:"include_temp"`
	IncludeRemoved bool     `json:"include_removed"`
}

// projectStatus says whether a project is already configured.
type projectStatus string

const (
	projectStatusWillBeAdded     projectStatus = "will_be_added"
	projectStatusAlreadyIncluded projectStatus = "already_included"
)

type projectJSON struct {
	Root           string         `json:"root"`
	Kind           ProjectKind    `json:"kind"`
	Status         projectStatus  `json:"status"`
	Exists         bool           `json:"exists"`
	Sessions       map[string]int `json:"sessions"`
	Subagents      int            `json:"subagents"`
	Bytes          int64          `json:"bytes"`
	FirstStartedAt time.Time      `json:"first_started_at"`
	LastStartedAt  time.Time      `json:"last_started_at"`
	// CapturesSubfolders is set when adding the project makes hooks capture
	// new sessions in folders under it that no nearer project owns.
	CapturesSubfolders bool `json:"captures_subfolders"`
	// KeptOut are the folders inside it the import adds as excluded
	// projects; KeptOutUnchecked are those among them kept out whole
	// without being looked in, because macOS protects them; KeptOutComplete
	// is false when not every other folder inside could be checked.
	KeptOut          []string `json:"kept_out"`
	KeptOutUnchecked []string `json:"kept_out_unchecked"`
	KeptOutComplete  bool     `json:"kept_out_complete"`
}

// RenderJSON writes the plan with the spec's top-level keys, for
// `backfill --dry-run --json`. It holds project roots, which are paths, but
// never a transcript path, a native or archive session ID, or content.
func RenderJSON(w io.Writer, p Plan) error {
	out := planJSON{
		Destination: p.Destination,
		Filters: filtersJSON{
			Harnesses: []string{}, Projects: []string{},
			Since: p.Filters.Since, Until: p.Filters.Until,
			IncludeHome: p.Filters.IncludeHome, IncludeTemp: p.Filters.IncludeTemp, IncludeRemoved: p.Filters.IncludeRemoved,
		},
		Projects:         []projectJSON{},
		Skipped:          p.Skipped(),
		AppsWithoutHooks: p.AppsWithoutHooks(),
		RetentionDays:    p.RetentionDays,
		StorageChecked:   false,
		// False when Cursor's database could not be read; see
		// CursorDatabaseReader.
		CursorDatabaseChecked:         p.CursorDatabaseChecked,
		CursorDatabaseUncheckedReason: p.CursorDatabaseUnchecked,
		CursorDatabaseNewerFormat:     p.CursorDatabaseNewerFormat,
		CursorSubagentsNotImported:    p.CursorSubagentsNotImported,
		SubagentsSkipped:              p.SubagentsSkipped(),
		UnreadableFolders:             p.UnreadableFolders,
		UnreadableStores:              append([]string{}, p.UnreadableStores...),
	}
	for _, h := range p.Filters.Harnesses {
		out.Filters.Harnesses = append(out.Filters.Harnesses, archive.CanonicalHarness(h))
	}
	for _, dir := range p.Filters.Projects {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		out.Filters.Projects = append(out.Filters.Projects, dir)
	}
	out.ExpiresOn, _ = p.ExpiresOn()
	for _, s := range p.Projects() {
		status := projectStatusWillBeAdded
		if s.Included {
			status = projectStatusAlreadyIncluded
		}
		sessions := map[string]int{}
		for _, h := range harnessOrder {
			sessions[h] = s.Sessions[h]
		}
		out.Projects = append(out.Projects, projectJSON{
			Root: s.Root, Kind: s.Kind, Status: status, Exists: s.Exists,
			Sessions: sessions, Subagents: s.Subagents, Bytes: s.Bytes,
			FirstStartedAt: s.FirstStart.UTC(), LastStartedAt: s.LastStart.UTC(),
			CapturesSubfolders: s.CapturesSubfolders(), KeptOut: append([]string{}, s.KeptOut...), KeptOutUnchecked: append([]string{}, s.KeptOutUnchecked...), KeptOutComplete: s.NestedComplete,
		})
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}
