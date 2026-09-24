package backfill

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
	Bytes                 int64
	FirstStart, LastStart time.Time
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

// Found is how many sessions discovery found, imported or not.
func (p Plan) Found() int {
	return len(p.Candidates) + p.CursorDatabaseOnly
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
	if p.CursorDatabaseOnly > 0 {
		if harnessMatches(p.Filters.Harnesses, "cursor") {
			counts[SkipCursorDatabaseOnly] += p.CursorDatabaseOnly
		} else {
			counts[SkipFilteredOut] += p.CursorDatabaseOnly
		}
	}
	return counts
}

// SubagentsSkipped counts the subagent transcripts of imported sessions that
// are left out because they are too large or the filter refuses them.
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
		if importing[h] && !containsString(p.Harnesses, h) {
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
		fmt.Fprintln(w, "Nothing to import.")
		renderSkipped(w, p)
		return
	}
	if p.Filters.Active() {
		fmt.Fprintf(w, "Backfill imports sessions matching %s\ninto %s. Nothing has been uploaded yet.\n", p.filterFlags(), p.destination())
	} else {
		fmt.Fprintf(w, "Backfill imports every session found on this Mac into\n%s. Nothing has been uploaded yet.\n", p.destination())
	}
	fmt.Fprintln(w)

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
	fmt.Fprintf(w, "%-*s%6s  %5s  %6s  %5s\n", width, "PROJECT", "CLAUDE", "CODEX", "CURSOR", "TOTAL")
	for _, s := range repos {
		p.renderRow(w, width, s)
	}
	if len(others) > 0 {
		if len(repos) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Not a repository:")
		for _, s := range others {
			p.renderRow(w, width, s)
		}
	}
	fmt.Fprintln(w)

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
	total := "Total: " + count(sessions, "session")
	if subagents > 0 {
		total += fmt.Sprintf(" (plus %s)", count(subagents, "subagent transcript"))
	}
	fmt.Fprintf(w, "%s, %s,\n", total, formatSize(bytes))
	loc := p.GeneratedAt.Location()
	firstDay, lastDay := first.In(loc).Format(dateLayout), last.In(loc).Format(dateLayout)
	if firstDay == lastDay {
		fmt.Fprintf(w, "       started %s.\n", firstDay)
	} else {
		fmt.Fprintf(w, "       started %s to %s.\n", firstDay, lastDay)
	}

	renderSkipped(w, p)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "If you continue:")
	added := 0
	for _, s := range projects {
		if !s.Included {
			added++
		}
	}
	var hooked []string
	for _, h := range harnessOrder {
		if containsString(p.Harnesses, h) {
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
		fmt.Fprintf(w, "  • %d %s added, and %s\n", added, verb, captured)
		if needSetup != "" {
			fmt.Fprintf(w, "    %s\n", needSetup)
		}
	case needSetup != "":
		fmt.Fprintf(w, "  • %s\n", needSetup)
	}
	if expires, ok := p.ExpiresOn(); ok {
		fmt.Fprintf(w, "  • Retention is %d days, so these sessions are deleted on %s.\n    Choose `edit` to keep them longer.\n", p.RetentionDays, expires)
	} else {
		fmt.Fprintln(w, "  • Retention is off, so these sessions are kept until you delete them.")
	}
	fmt.Fprintln(w, "  • Undo any time with `agent-archive backfill undo`.")
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
	fmt.Fprintf(w, "%-*s%6s  %5s  %6s  %5d  %s\n", width, p.rowLabel(s), cells[0], cells[1], cells[2], s.Total(), status)
	switch s.Kind {
	case ProjectKindScratch:
		if p.isCodexWorkspaces(s.Root) {
			fmt.Fprintln(w, "  Chats in the workspaces Codex creates for them. New ones will be captured too.")
		} else {
			fmt.Fprintln(w, "  Chats started without a folder. New ones will be captured too.")
		}
	case ProjectKindHome:
		if s.Included {
			break
		}
		fmt.Fprintln(w, "  Every future session under your home folder that isn't in a nearer project")
		fmt.Fprintln(w, "  will be captured too.")
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
	return fmt.Sprint(n)
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
	SkipTooLarge:              "larger than 64 MiB",
	SkipStartInFuture:         "starting in the future (check the clock)",
	SkipCursorDatabaseOnly:    "%s stored only in Cursor's database (a later release)",
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
	if len(counts) == 0 && subagentsSkipped == 0 && !databaseUnchecked {
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
		n               int
		label, override string
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
			apps := nouns[reason]
			if reason == SkipCursorDatabaseOnly {
				apps = map[string]bool{"cursor": true}
			}
			label = fmt.Sprintf(label, sessionNoun(apps, n))
		}
		lines = append(lines, line{n, label, skipOverrides[reason]})
		if skipOverrides[reason] != "" {
			width = max(width, utf8.RuneCountInString(label))
		}
	}
	if subagentsSkipped > 0 {
		label := "subagent transcripts that can't be read"
		if subagentsSkipped == 1 {
			label = "subagent transcript that can't be read"
		}
		lines = append(lines, line{subagentsSkipped, label, ""})
	}
	width = max(width+2, 42)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Not imported:")
	for _, l := range lines {
		if l.override == "" {
			fmt.Fprintf(w, "%4d  %s\n", l.n, l.label)
		} else {
			fmt.Fprintf(w, "%4d  %-*s%s\n", l.n, width, l.label, l.override)
		}
	}
	if databaseUnchecked {
		fmt.Fprintln(w, "      Cursor chats stored only in Cursor's database were not checked")
		fmt.Fprintln(w, "      (a later release).")
	}
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
		flags = append(flags, "--harness "+canonicalHarness(h))
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

func count(n int, noun string) string {
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

// formatSize shows a byte count in decimal units, as Finder does.
func formatSize(n int64) string {
	switch {
	case n < 1000:
		return count(int(n), "byte")
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
	ExpiresOn             string `json:"expires_on"`
	StorageChecked        bool   `json:"storage_checked"`
	CursorDatabaseChecked bool   `json:"cursor_database_checked"`
	SubagentsSkipped      int    `json:"subagents_skipped"`
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

type projectJSON struct {
	Root           string         `json:"root"`
	Kind           ProjectKind    `json:"kind"`
	Status         string         `json:"status"`
	Exists         bool           `json:"exists"`
	Sessions       map[string]int `json:"sessions"`
	Subagents      int            `json:"subagents"`
	Bytes          int64          `json:"bytes"`
	FirstStartedAt time.Time      `json:"first_started_at"`
	LastStartedAt  time.Time      `json:"last_started_at"`
}

// RenderJSON writes the plan with the spec's top-level keys. storageChecked
// says whether storage access was verified before planning.
func RenderJSON(w io.Writer, p Plan, storageChecked bool) error {
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
		StorageChecked:   storageChecked,
		// Phase 1 cannot read Cursor's database yet; see
		// Environment.CursorDatabaseOnly.
		CursorDatabaseChecked: p.CursorDatabaseChecked,
		SubagentsSkipped:      p.SubagentsSkipped(),
	}
	for _, h := range p.Filters.Harnesses {
		out.Filters.Harnesses = append(out.Filters.Harnesses, canonicalHarness(h))
	}
	for _, dir := range p.Filters.Projects {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		out.Filters.Projects = append(out.Filters.Projects, dir)
	}
	out.ExpiresOn, _ = p.ExpiresOn()
	for _, s := range p.Projects() {
		status := "will_be_added"
		if s.Included {
			status = "already_included"
		}
		sessions := map[string]int{}
		for _, h := range harnessOrder {
			sessions[h] = s.Sessions[h]
		}
		out.Projects = append(out.Projects, projectJSON{
			Root: s.Root, Kind: s.Kind, Status: status, Exists: s.Exists,
			Sessions: sessions, Subagents: s.Subagents, Bytes: s.Bytes,
			FirstStartedAt: s.FirstStart.UTC(), LastStartedAt: s.LastStart.UTC(),
		})
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}
