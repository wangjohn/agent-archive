package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Plan is everything one backfill run found and decided. It is the input the
// import step registers from; its rendering never shows a transcript path,
// native ID, or content.
type Plan struct {
	GeneratedAt time.Time
	// Home is the user's home directory, for showing paths with ~.
	Home          string
	Filters       Filters
	Destination   Destination
	RetentionDays int
	// Harnesses are the apps with hooks installed (config.Config.Harnesses).
	Harnesses  []string
	Candidates []Candidate
	// CursorDatabaseOnly counts Cursor chats found only in Cursor's database,
	// when CursorDatabaseChecked says the count was available;
	// CursorDatabaseUnchecked says why it was not. CursorDatabaseFiltered
	// counts those --since, --until, or --project leave out, and
	// CursorDatabaseSkipped those the archive already knows, by reason.
	CursorDatabaseOnly      int
	CursorDatabaseFiltered  int
	CursorDatabaseSkipped   map[SkipReason]int
	CursorDatabaseChecked   bool
	CursorDatabaseUnchecked CursorUncheckedReason
	// CursorDatabaseNewerFormat counts the database rows read with a newer
	// format version than this release knows.
	CursorDatabaseNewerFormat int

	// resolvedHome is Home with symlinks resolved; roots are resolved paths.
	resolvedHome string
	// projectFilter holds the --project directories, resolved as the plan
	// compared them.
	projectFilter []string
}

// Destination names the bucket imports go to.
type Destination struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}

// workers is the spec's filter worker count: filtering is CPU-bound, and
// using every core would cost the person's machine more than it saves.
func defaultWorkers() int {
	return min(8, max(2, runtime.NumCPU()/2))
}

// maxBytesInFlight caps the transcript bytes being filtered at once, so two
// files near the 64 MiB limit are never read together. A file larger than
// the cap runs alone.
const maxBytesInFlight = 128 << 20

// work is one transcript and the facts the plan decides its reason from.
type work struct {
	t   *transcript
	c   Candidate
	res resolution
	// state is the archive's reason, from ArchiveState.
	state SkipReason
	// vanished is set when the file disappeared while planning; the session
	// is then not counted at all.
	vanished bool
	filtered bool
	// duplicated is set when another file carries the same harness and
	// native ID; duplicate when this file is not the one kept.
	duplicated, duplicate bool
	// Adapter outcomes.
	empty, unsafe, tooLarge bool
}

// subagentWork is one subagent transcript of an imported parent.
type subagentWork struct {
	parent            *work
	sub               Subagent
	skipped, vanished bool
}

// importable reports whether nothing about the file itself stops it being
// imported: the checks a session's copies can differ on.
func (w *work) importable() bool {
	return !w.tooLarge && !w.unsafe && !w.empty && !w.t.identityMismatch
}

// markDuplicates keeps one file of a session found more than once and marks
// the rest. The kept file is one that can be imported, so a larger copy that
// is too large, empty, or refused never displaces a good one; then one whose
// own IDs agree, then a Codex file in sessions/ over archived_sessions/, then
// the larger file, then the lexically smallest path.
func markDuplicates(env Environment, group []*work) {
	var live []*work
	for _, w := range group {
		if !w.vanished {
			live = append(live, w)
		}
	}
	if len(live) < 2 {
		return
	}
	active := filepath.Join(env.Home, ".codex", "sessions") + string(filepath.Separator)
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.importable() != b.importable() {
			return a.importable()
		}
		if a.t.identityMismatch != b.t.identityMismatch {
			return !a.t.identityMismatch
		}
		if aActive, bActive := strings.HasPrefix(a.t.path, active), strings.HasPrefix(b.t.path, active); aActive != bActive {
			return aActive
		}
		if a.t.size != b.t.size {
			return a.t.size > b.t.size
		}
		return a.t.path < b.t.path
	})
	for _, w := range live[1:] {
		w.duplicate = true
	}
}

// BuildPlan finds every session on this Mac and decides, for each, whether it
// is imported or why not. It writes nothing.
func BuildPlan(ctx context.Context, env Environment, state ArchiveState, cfg config.Config, filters Filters) (Plan, error) {
	if err := filters.Validate(); err != nil {
		return Plan{}, err
	}
	now := env.now()
	plan := Plan{
		GeneratedAt:   now,
		Home:          env.Home,
		Filters:       filters,
		Destination:   Destination{Provider: cfg.Storage.Provider, Bucket: cfg.Storage.Bucket, Prefix: cfg.Storage.Prefix},
		RetentionDays: cfg.RetentionDays,
		Harnesses:     append([]string(nil), cfg.Harnesses...),
		resolvedHome:  env.resolved(env.Home),
	}
	found, err := discover(env)
	if err != nil {
		return Plan{}, fmt.Errorf("find sessions: %w", err)
	}
	workers := env.Workers
	if workers <= 0 {
		workers = defaultWorkers()
	}

	items := make([]*work, len(found))
	for i, t := range found {
		items[i] = &work{t: t, c: Candidate{Harness: t.harness, TranscriptPath: t.path, Bytes: t.size}}
	}
	// Identity and working directory come from each transcript's leading
	// records.
	if err := forEach(ctx, workers, items, func(w *work) {
		if err := readHead(env, w.t); err != nil {
			if isNotExist(err) {
				w.vanished = true
			} else {
				w.unsafe = true
			}
		}
		w.c.NativeSessionID = w.t.nativeID
	}); err != nil {
		return Plan{}, err
	}

	// Projects. Cursor's come last: its slugs are matched against the roots
	// the other apps' sessions resolved to.
	r := newResolver(env, cfg, filters)
	var cursorCandidates []string
	for _, p := range cfg.Archive.Projects {
		cursorCandidates = append(cursorCandidates, p.Root)
	}
	for _, w := range items {
		if w.t.harness == "cursor" || w.vanished {
			continue
		}
		w.res = r.resolve(w.t.cwd)
		if w.t.cwd != "" {
			cursorCandidates = append(cursorCandidates, w.t.cwd)
		}
		if w.res.root != "" {
			cursorCandidates = append(cursorCandidates, w.res.root)
		}
	}
	matcher := newCursorMatcher(env, cursorCandidates)
	for _, w := range items {
		if w.t.harness != "cursor" {
			continue
		}
		if folder, ok := matcher.match(w.t.cursorSlug); ok {
			w.res = r.resolve(folder)
		} else {
			w.res = resolution{skip: SkipProjectUnknown}
		}
	}

	projectFilter := make([]string, 0, len(filters.Projects))
	for _, p := range filters.Projects {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		projectFilter = append(projectFilter, env.resolved(p))
	}
	plan.projectFilter = projectFilter
	since, until := dateRange(filters, now.Location())

	sessions := map[string][]*work{}
	for _, w := range items {
		if w.vanished {
			continue
		}
		if strings.TrimSpace(w.c.NativeSessionID) != "" {
			reason, err := state.Classify(w.t.harness, w.c.NativeSessionID)
			if err != nil {
				return Plan{}, fmt.Errorf("check the archive: %w", err)
			}
			if filters.IncludeRemoved && (reason == SkipRemovedByUndo || reason == SkipRemovedByRetention) {
				reason = ""
			}
			w.state = reason
			key := w.t.harness + "\x00" + w.c.NativeSessionID
			sessions[key] = append(sessions[key], w)
		}
		w.filtered = !harnessMatches(filters.Harnesses, w.t.harness) || !projectMatches(env, projectFilter, w.res.root)
		w.tooLarge = w.t.size > archive.MaxRecordBytes
	}
	for _, group := range sessions {
		if len(group) > 1 {
			for _, w := range group {
				w.duplicated = true
			}
		}
	}

	// The adapter runs over every transcript that may be imported, as the
	// collector will, so the plan's counts are what gets imported. A session
	// already decided by an earlier reason is not read, unless a date filter
	// needs its start or a duplicate needs its identity checked.
	dated := !since.IsZero() || !until.IsZero()
	var toFilter []*work
	for _, w := range items {
		if w.vanished || w.state == SkipAlreadyArchived || w.tooLarge || w.unsafe {
			continue
		}
		if w.duplicated || (w.state == "" && !w.filtered && (dated || (w.res.skip == "" && !w.t.identityMismatch))) {
			toFilter = append(toFilter, w)
		}
	}
	budget := newByteBudget(maxBytesInFlight)
	if err := forEach(ctx, workers, toFilter, func(w *work) {
		n := budget.acquire(w.t.size)
		defer budget.release(n)
		runAdapter(env, w)
	}); err != nil {
		return Plan{}, err
	}
	for _, group := range sessions {
		markDuplicates(env, group)
	}

	var parents []*work
	for _, w := range items {
		if w.vanished {
			continue
		}
		if w.c.StartedAt.IsZero() && w.state == "" {
			// No record carried a timestamp, or the file was not read: the
			// file's creation is the best start available.
			if created, err := env.fileCreated(w.t.path); err == nil {
				w.c.StartedAt, w.c.StartedAtSource = created.UTC(), archive.StartedAtSourceFileCreated
			}
		}
		if dated && w.state == "" && !inRange(w.c.StartedAt, since, until) {
			w.filtered = true
		}
		w.c.ProjectRoot, w.c.ProjectKind, w.c.ProjectIncluded = w.res.root, w.res.kind, w.res.included
		if w.res.root != "" {
			w.c.ProjectExists = env.exists(w.res.root)
		}
		w.c.Skip = w.reason(now)
		if w.c.Skip == "" && w.t.harness == "claude" {
			parents = append(parents, w)
		}
	}

	// Subagent transcripts of imported parents pass the same checks as their
	// parents; one that fails is left out and counted.
	var subagents []*subagentWork
	for _, w := range parents {
		for _, sub := range claudeSubagents(env, w.t) {
			subagents = append(subagents, &subagentWork{parent: w, sub: sub})
		}
	}
	if err := forEach(ctx, workers, subagents, func(s *subagentWork) {
		if s.sub.Bytes > archive.MaxRecordBytes {
			s.skipped = true
			return
		}
		n := budget.acquire(s.sub.Bytes)
		defer budget.release(n)
		if _, _, err := collector.FilterTranscriptFile("claude", s.sub.Path, time.Time{}); err != nil {
			s.vanished = isNotExist(err)
			s.skipped = !s.vanished
		}
	}); err != nil {
		return Plan{}, err
	}
	for _, s := range subagents {
		switch {
		case s.vanished:
		case s.skipped:
			s.parent.c.SubagentsSkipped++
		default:
			s.parent.c.Subagents = append(s.parent.c.Subagents, s.sub)
		}
	}
	for _, w := range items {
		if !w.vanished {
			plan.Candidates = append(plan.Candidates, w.c)
		}
	}

	if err := countCursorDatabase(ctx, env, state, r, projectFilter, since, until, &plan); err != nil {
		return Plan{}, err
	}
	sort.SliceStable(plan.Candidates, func(i, j int) bool {
		a, b := plan.Candidates[i], plan.Candidates[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return a.TranscriptPath < b.TranscriptPath
	})
	return plan, nil
}

// reason picks the first applicable skip reason in the spec's order.
func (w *work) reason(now time.Time) SkipReason {
	applies := map[SkipReason]bool{
		w.state:    w.state != "",
		w.res.skip: w.res.skip != "",
	}
	for reason, set := range map[SkipReason]bool{
		SkipDuplicateSession: w.duplicate,
		SkipFilteredOut:      w.filtered,
		SkipIdentityMismatch: w.t.identityMismatch,
		SkipEmpty:            w.empty,
		// A session that would register without a start time cannot be
		// imported: the registration requires one.
		SkipUnsafeFormat:  w.unsafe || (w.state == "" && w.c.StartedAt.IsZero()),
		SkipTooLarge:      w.tooLarge,
		SkipStartInFuture: w.c.StartedAt.After(now),
	} {
		applies[reason] = applies[reason] || set
	}
	for _, reason := range skipOrder {
		if applies[reason] {
			return reason
		}
	}
	return ""
}

// runAdapter filters the whole transcript with the collector's own code and
// keeps only what the plan needs: whether anything is left, the IDs the
// records carry, and the earliest record's time.
func runAdapter(env Environment, w *work) {
	var freshStart time.Time
	if w.t.harness == "cursor" {
		// Cursor records carry no timestamps; the file's creation is the
		// start, and the text filter needs it as its fresh-start proof.
		created, err := env.fileCreated(w.t.path)
		if err != nil {
			if isNotExist(err) {
				w.vanished = true
			} else {
				w.unsafe = true
			}
			return
		}
		freshStart = created.UTC()
		w.c.StartedAt, w.c.StartedAtSource = freshStart, archive.StartedAtSourceFileCreated
	}
	filtered, _, err := collector.FilterTranscriptFile(w.t.harness, w.t.path, freshStart)
	if err != nil {
		info, statErr := env.lstat(w.t.path)
		switch {
		case isNotExist(err) || isNotExist(statErr):
			w.vanished = true
		case errors.Is(err, archive.ErrRecordTooLarge) || (statErr == nil && info.Size() > archive.MaxRecordBytes):
			// One record over the limit, or a file that grew past it since
			// discovery: the collector blocks it as too large.
			w.tooLarge = true
		default:
			// The collector would refuse it too, and block the registration
			// for good.
			w.unsafe = true
		}
		return
	}
	w.empty = !carriesConversation(filtered)
	switch w.t.harness {
	case "claude":
		// The file stem is the ID hooks register. A fork or resume can copy
		// records carrying an earlier session's ID, so the stem must be among
		// the records' IDs rather than the only one.
		if len(filtered.SessionIDs) > 0 && !containsString(filtered.SessionIDs, w.t.nativeID) {
			w.t.identityMismatch = true
		}
		if !filtered.NativeStartAt.IsZero() {
			w.c.StartedAt, w.c.StartedAtSource = filtered.NativeStartAt.UTC(), archive.StartedAtSourceTranscript
		}
	case "codex":
		switch {
		case !w.t.metaStart.IsZero():
			w.c.StartedAt, w.c.StartedAtSource = w.t.metaStart, archive.StartedAtSourceTranscript
		case !filtered.NativeStartAt.IsZero():
			w.c.StartedAt, w.c.StartedAtSource = filtered.NativeStartAt.UTC(), archive.StartedAtSourceTranscript
		}
	}
}

// conversationTypes are the retained record types that hold a turn of the
// conversation, as opposed to bookkeeping (summaries, turn context, session
// metadata).
var conversationTypes = map[string]bool{
	"user": true, "assistant": true, "message": true, "response_item": true,
	"tool_use": true, "tool_result": true, "tool_call": true,
}

// carriesConversation reports whether a filtered transcript holds anything a
// person or agent said. A transcript without it would become a registration
// that never publishes anything.
func carriesConversation(filtered archive.FilteredTranscript) bool {
	for _, text := range filtered.Text {
		if strings.TrimSpace(text) != "" {
			return true
		}
	}
	for _, record := range filtered.Records {
		var r struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(record, &r) == nil && (conversationTypes[r.Type] || r.Role != "") {
			return true
		}
	}
	return false
}

// claudeSubagents lists <slug>/<session>/subagents/agent-<id>.jsonl for an
// imported Claude Code parent.
func claudeSubagents(env Environment, t *transcript) []Subagent {
	dir := filepath.Join(filepath.Dir(t.path), t.nativeID, "subagents")
	entries, err := readDirIfExists(env, dir)
	if err != nil {
		return nil
	}
	var subagents []Subagent
	for _, e := range entries {
		if !e.regular || !strings.HasPrefix(e.name, "agent-") || !strings.HasSuffix(e.name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.name)
		if size, ok := fileSize(env, path); ok {
			id := strings.TrimSuffix(strings.TrimPrefix(e.name, "agent-"), ".jsonl")
			subagents = append(subagents, Subagent{Path: path, AgentID: id, Bytes: size})
		}
	}
	return subagents
}

func harnessMatches(harnesses []string, harness string) bool {
	if len(harnesses) == 0 {
		return true
	}
	for _, h := range harnesses {
		if canonicalHarness(h) == harness {
			return true
		}
	}
	return false
}

// projectMatches compares a session's project root with the --project
// directories, both resolved. A session without a project matches none.
func projectMatches(env Environment, projects []string, root string) bool {
	if len(projects) == 0 {
		return true
	}
	if root == "" {
		return false
	}
	root = env.resolved(root)
	for _, p := range projects {
		if p == root {
			return true
		}
	}
	return false
}

// dateRange turns --since and --until into [since, until) in loc.
func dateRange(f Filters, loc *time.Location) (since, until time.Time) {
	if f.Since != "" {
		since, _ = time.ParseInLocation(dateLayout, f.Since, loc)
	}
	if f.Until != "" {
		if day, err := time.ParseInLocation(dateLayout, f.Until, loc); err == nil {
			until = day.AddDate(0, 0, 1)
		}
	}
	return since, until
}

func inRange(t, since, until time.Time) bool {
	if t.IsZero() {
		return false
	}
	return (since.IsZero() || !t.Before(since)) && (until.IsZero() || t.Before(until))
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// forEach runs fn over items with a fixed number of workers.
func forEach[T any](ctx context.Context, workers int, items []T, fn func(T)) error {
	jobs := make(chan T)
	var wg sync.WaitGroup
	for range min(workers, max(1, len(items))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				fn(w)
			}
		}()
	}
	var err error
	for _, w := range items {
		if err = ctx.Err(); err != nil {
			break
		}
		jobs <- w
	}
	close(jobs)
	wg.Wait()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// byteBudget is a counting semaphore over bytes.
type byteBudget struct {
	mu        sync.Mutex
	available *sync.Cond
	limit     int64
	used      int64
}

func newByteBudget(limit int64) *byteBudget {
	b := &byteBudget{limit: limit}
	b.available = sync.NewCond(&b.mu)
	return b
}

// acquire waits until n bytes fit and returns what it took; a request larger
// than the whole budget takes all of it, so that file runs alone.
func (b *byteBudget) acquire(n int64) int64 {
	n = min(max(n, 0), b.limit)
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+n > b.limit {
		b.available.Wait()
	}
	b.used += n
	return n
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.available.Broadcast()
}
