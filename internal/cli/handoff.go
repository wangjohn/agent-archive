package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// handoffDir is the data-directory entry holding untrimmed handoffs saved when
// the budget trimmed the printed one, and the directories holding the copies
// launched agents read (launch-*/). uninstall's localStateEntries lists it.
const handoffDir = "handoffs"

// handoffMaxAge is how long a saved full handoff is kept.
const handoffMaxAge = 7 * 24 * time.Hour

// handoffFallbackRows is how many recent sessions `--latest` lists when it
// finds no session for the project.
const handoffFallbackRows = 5

// handoffTarget is one resolved session: its filtered bundle, its metadata
// when it came from the archive, and where it came from.
type handoffTarget struct {
	bundle   archive.SourceBundle
	metadata *archive.Metadata
	source   string
	filePath string
	// describe is the stderr line naming a --latest choice.
	describe string
	// startedAt and lastActivityAt are what this machine knows about a local
	// session (its registration, its transcript's modification time), used
	// only where the transcript records no timestamps.
	startedAt      time.Time
	lastActivityAt time.Time
}

// currentSessionEnv names environment variables an agent sets for the commands
// it runs, holding its own native session ID, and the harness that sets each.
// `--latest` skips that session: run from inside an agent, the newest session
// is always the one asking. `--to` with no selector hands it off instead.
// CLAUDE_CODE_SESSION_ID is observed in Claude Code; CODEX_THREAD_ID is read
// if present but has not been observed.
var currentSessionEnv = []struct {
	key     string
	harness string
}{
	{"CLAUDE_CODE_SESSION_ID", archive.HarnessClaude},
	{"CODEX_THREAD_ID", archive.HarnessCodex},
}

var errHandoffNotSetUp = errors.New("handoff not set up")

// runHandoffCommand implements `agent-archive handoff`. It prints transcript
// content, which the command itself is the explicit request for; every
// rendered byte comes from a filtered bundle, whether that bundle was
// downloaded or built in memory from a local transcript.
func runHandoffCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env handoffCommandDependencies) int {
	interactive := browseInteractive(env, stdin, stdout)
	opts, ok := parseHandoffOptions(args, stderr, env, interactive)
	if !ok {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: resolve home: %v\n", err)
		return 1
	}
	// Every prompt reads through one buffer, so an answer typed (or
	// scripted) ahead for a later prompt is not lost to an earlier one's.
	// A launched agent gets stdin itself: it must see the terminal.
	answers := bufio.NewReader(stdin)
	if opts.sessionID == "" && !opts.latest && opts.file == "" {
		if code, done := chooseHandoffSession(&opts, home, interactive, answers, stdout, stderr, env); done {
			return code
		}
	}
	target, err := resolveHandoffTarget(opts, home, stderr, env)
	if err != nil {
		if errors.Is(err, errHandoffNotSetUp) {
			terminal.Println(stderr, notSetUpMessage)
		} else {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		}
		return 1
	}
	if target.describe != "" {
		terminal.Printf(stderr, "handoff: using %s\n", target.describe)
	}
	h, err := archive.BuildHandoff(target.bundle, target.metadata, archive.HandoffOptions{Source: target.source, StartedAt: target.startedAt, LastActivityAt: target.lastActivityAt})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		return 1
	}
	rendered := prepareHandoff(h, target.bundle, opts, home, stderr, env)
	dest := handoffDestination(opts.to)
	if offersDestinations(opts, interactive) {
		p := newPrompter(answers, stdout)
		choice, err := askHandoffDestination(p, h, home, env)
		if err == nil && choice.action != handoffLaunch {
			err = deliverHandoff(choice, p, rendered, target, opts, stdout, stderr, env)
		}
		if err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1
		}
		if choice.action != handoffLaunch {
			return 0
		}
		dest = choice.dest
	}
	if dest != "" {
		// Run from inside an agent, or asked to, the new agent gets a
		// terminal of its own.
		here := interactive && !opts.newWindow
		if err := launchPreparedHandoff(rendered, h, target, dest, here, opts, home, stdin, stdout, stderr, env); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeHandoffResult(rendered, opts, stdout, stderr); err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		return 1
	}
	return 0
}

func resolveHandoffTarget(opts handoffOptions, home string, stderr io.Writer, env handoffTargetDependencies) (handoffTarget, error) {
	if opts.file != "" {
		return handoffFromFile(opts.file, opts.harness, env)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return handoffTarget{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return handoffTarget{}, errHandoffNotSetUp
	}
	skip := currentSessions(env)
	if opts.to != "" {
		// The calling agent is the source of a direct handoff.
		skip = nil
	}
	resolver := handoffResolver{ctx: context.Background(), env: env, home: home, cfg: cfg,
		harness: opts.harness, source: opts.source, skip: skip, stderr: stderr}
	if !opts.latest {
		return resolver.byID(opts.sessionID)
	}
	dir := opts.project
	if dir == "" {
		dir, err = env.workingDir()
	}
	if err != nil {
		return handoffTarget{}, err
	}
	return resolver.latest(dir)
}

func renderHandoff(h archive.Handoff, opts handoffOptions) []byte {
	if opts.format == "json" {
		data, _ := json.MarshalIndent(h, "", "  ")
		return append(archive.DisplayJSON(data), '\n')
	}
	return archive.RenderHandoffMarkdown(h, archive.HandoffRenderOptions{Preamble: !opts.noPreamble})
}

type handoffRenderPlan struct {
	fullPath string
	full     []byte
	fitted   archive.Handoff
	fits     bool
}

// planHandoffRendering decides what to trim and where an untrimmed copy
// would live. It leaves saving that copy to prepareHandoff.
func planHandoffRendering(h archive.Handoff, bundle archive.SourceBundle, opts handoffOptions, home string) handoffRenderPlan {
	fullPath := handoffFullPath(home, bundle, opts.format)
	full := renderHandoff(h, opts)
	h.FullRecordPath = fullPath
	fitted, fits := archive.FitHandoff(h, opts.maxBytes, func(h archive.Handoff) int { return len(renderHandoff(h, opts)) })
	return handoffRenderPlan{fullPath: fullPath, full: full, fitted: fitted, fits: fits}
}

func prepareHandoff(h archive.Handoff, bundle archive.SourceBundle, opts handoffOptions, home string, stderr io.Writer, env handoffFileDependencies) []byte {
	pruneHandoffs(home, env.now())
	plan := planHandoffRendering(h, bundle, opts, home)
	fitted := plan.fitted
	if len(fitted.Elisions) > 0 {
		// The data directory exists once setup has run. `--file` works
		// without setup, and must not create it just to hold a copy of a
		// transcript nobody opted in to archiving.
		if _, statErr := os.Stat(home); statErr != nil {
			terminal.Println(stderr, "agent-archive: handoff: note: output was trimmed and, without setup, the untrimmed version is not saved; use --max-bytes 0 for all of it")
			fitted.FullRecordPath = ""
		} else if err := local.WriteBytes(plan.fullPath, plan.full); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: warning: could not save the untrimmed handoff: %v\n", err)
			fitted.FullRecordPath = ""
		}
	} else {
		fitted.FullRecordPath = ""
	}
	rendered := renderHandoff(fitted, opts)
	if !plan.fits {
		terminal.Printf(stderr, "agent-archive: handoff: warning: still %d bytes after trimming, over the %d-byte limit\n", len(rendered), opts.maxBytes)
	}
	return rendered
}

func writeHandoffResult(rendered []byte, opts handoffOptions, stdout, stderr io.Writer) error {
	if opts.output == "" {
		if _, err := stdout.Write(rendered); err != nil {
			return err
		}
		return nil
	}
	if err := writeHandoffOutput(opts.output, rendered, opts.force); err != nil {
		return err
	}
	terminal.Printf(stderr, "handoff: wrote %s (%d bytes)\n", opts.output, len(rendered))
	return nil
}

func (env Env) workingDir() (string, error) {
	if env.WorkingDir != nil {
		return env.WorkingDir()
	}
	return os.Getwd()
}

// handoffFromFile filters a native transcript that may have no registration.
// Its modification time stands in for a start time, which only Cursor's text
// fallback needs.
func handoffFromFile(path, harness string, env handoffFileDependencies) (handoffTarget, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return handoffTarget{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return handoffTarget{}, err
	}
	filtered, adapter, err := collector.FilterTranscriptFile(harness, abs, info.ModTime())
	if err != nil {
		return handoffTarget{}, fmt.Errorf("filter %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(abs))
	nativeID := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	if len(filtered.SessionIDs) > 0 {
		nativeID = filtered.SessionIDs[0]
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: "file-" + hex.EncodeToString(sum[:])[:16],
		NativeSessionID:  nativeID,
		ProjectID:        "file",
		ProjectRoot:      filepath.Dir(abs),
		Harness:          archive.Harness{Name: adapter.Name()},
		SessionStartedAt: info.ModTime(),
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, env.now().UTC(), nil)
	if err != nil {
		return handoffTarget{}, err
	}
	return handoffTarget{bundle: bundle, source: "file", filePath: abs, lastActivityAt: info.ModTime()}, nil
}

// errNotRegisteredHere means a session ID has no registration on this machine,
// which is expected for a session captured elsewhere and not worth reporting
// beside an archive error.
var errNotRegisteredHere = errors.New("no session registered on this machine")

// currentSessions returns the native session IDs of the agent this command is
// running inside, if it says.
func currentSessions(env currentSessionDependencies) map[string]bool {
	ids := map[string]bool{}
	for _, v := range currentSessionEnv {
		if value, ok := env.lookupEnv(v.key); ok && strings.TrimSpace(value) != "" {
			ids[strings.TrimSpace(value)] = true
		}
	}
	return ids
}

// localTarget builds a handoff target from a registration's transcript.
func (r handoffResolver) localTarget(reg archive.SessionRegistration) (handoffTarget, error) {
	bundle, err := collector.ReadLocalBundle(r.ctx, r.home, reg, r.env.now().UTC(), r.env.cursorDatabase())
	if err != nil {
		return handoffTarget{}, err
	}
	lastActivityAt, _ := collector.LastActivity(r.ctx, reg, r.env.cursorDatabase())
	return handoffTarget{bundle: bundle, source: "local", startedAt: reg.SessionStartedAt, lastActivityAt: lastActivityAt}, nil
}

// hasPrompt reports whether a bundle holds anything the person said, so
// `--latest` passes over a session that has only just started.
func hasPrompt(bundle archive.SourceBundle) bool {
	_, ok := firstPrompt(bundle)
	return ok
}

// sessionTitleWidth is how many columns firstPrompt keeps, as the archive's
// metadata title does.
const sessionTitleWidth = 72

// firstPrompt is hasPrompt with a one-line preview of the first prompt, when
// the transcript's structure shows one (a Cursor text transcript's does not).
func firstPrompt(bundle archive.SourceBundle) (title string, ok bool) {
	if len(bundle.NativeText) > 0 {
		return "", true
	}
	view, err := archive.ParseNormalized(bundle)
	if err != nil {
		return "", false
	}
	for _, turn := range view.Turns {
		if turn.Kind == archive.TurnKindHumanPrompt {
			return ellipsize(strings.Join(strings.Fields(turn.Text), " "), sessionTitleWidth), true
		}
	}
	return "", false
}

type handoffResolver struct {
	ctx     context.Context
	env     handoffResolverDependencies
	home    string
	cfg     config.Config
	harness string
	source  string
	// skip holds native session IDs `--latest` must pass over: the agent
	// session running the command.
	skip map[string]bool
	// stderr receives warnings, such as a skipped metadata sidecar.
	stderr io.Writer
}

// byID resolves an explicit archive session ID: the local registration first
// (fresh, and available while paused or offline), then the archive.
func (r handoffResolver) byID(id string) (handoffTarget, error) {
	// localErr is why the local transcript could not be used; with --source
	// auto the archive's published copy is tried next, and both reasons are
	// reported if it fails too.
	var localErr error
	if r.source != "archive" {
		reg, found, err := state.OpenReadOnly(r.home).LoadRegistration(id)
		switch {
		case err != nil:
			localErr = err
		case found && (r.harness == "" || reg.Harness.Name == r.harness):
			target, err := r.localTarget(reg)
			if err == nil {
				return target, nil
			}
			localErr = err
		default:
			localErr = fmt.Errorf("%w: %q", errNotRegisteredHere, id)
		}
		if r.source == "local" {
			return handoffTarget{}, localErr
		}
	}
	target, err := r.archiveByID(id)
	if err != nil && localErr != nil && !errors.Is(localErr, errNotRegisteredHere) {
		return handoffTarget{}, fmt.Errorf("local transcript: %w; archive: %w", localErr, err)
	}
	return target, err
}

func (r handoffResolver) archiveByID(id string) (handoffTarget, error) {
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return handoffTarget{}, fmt.Errorf("open storage: %w", err)
	}
	key, err := locateMetadataKey(r.ctx, store, r.harness, id)
	if err != nil {
		return handoffTarget{}, err
	}
	return r.fromArchive(store, key, "")
}

func (r handoffResolver) fromArchive(store storage.ObjectStore, key, describe string) (handoffTarget, error) {
	metadata, bundle, err := reader.RefreshAndLoad(r.ctx, store, key, reader.Limits{})
	if err != nil {
		if errors.Is(err, reader.ErrRefreshRequired) {
			return handoffTarget{}, errors.New("the session's source bundle is not available (it may have just been replaced or deleted by retention); retry")
		}
		return handoffTarget{}, err
	}
	return handoffTarget{bundle: bundle, metadata: &metadata, source: "archive", describe: describe}, nil
}

// latest resolves `--latest`: the most recently active local registration
// for the project, then the most recently captured archived session with the
// project's ID, then an error listing recent sessions to choose from.
func (r handoffResolver) latest(dir string) (handoffTarget, error) {
	dir = filepath.Clean(dir)
	now := r.env.now()
	if r.source != "archive" {
		target, found, err := r.latestLocal(dir, now)
		if err != nil {
			return handoffTarget{}, err
		}
		if found {
			return target, nil
		}
	}
	return r.latestArchive(dir, now)
}

type localHandoffCandidate struct {
	reg    archive.SessionRegistration
	active time.Time
}

// localCandidates reads activity only for registrations that match the
// requested project and harness. Sorting is stable for equal activity times.
func (r handoffResolver) localCandidates(regs []archive.SessionRegistration, dir string) []localHandoffCandidate {
	var candidates []localHandoffCandidate
	for _, reg := range regs {
		if reg.ParentSessionID != "" || reg.SubagentID != "" || r.skip[reg.NativeSessionID] || !sameProject(reg.ProjectRoot, dir) {
			continue
		}
		if r.harness != "" && reg.Harness.Name != r.harness {
			continue
		}
		active := reg.RegisteredAt
		if at, ok := collector.LastActivity(r.ctx, reg, r.env.cursorDatabase()); ok {
			active = at
		}
		candidates = append(candidates, localHandoffCandidate{reg, active})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].active.After(candidates[j].active) })
	return candidates
}

func (r handoffResolver) latestLocal(dir string, now time.Time) (handoffTarget, bool, error) {
	regs, err := state.OpenReadOnly(r.home).LoadRegistrations()
	if err != nil {
		return handoffTarget{}, false, err
	}
	// A candidate without a readable transcript and a prompt is passed over.
	var skipped []string
	for _, c := range r.localCandidates(regs, dir) {
		target, err := r.localTarget(c.reg)
		if err == nil && !hasPrompt(target.bundle) {
			err = errors.New("no prompt yet")
		}
		if err != nil {
			if !errors.Is(err, collector.ErrNoTranscript) {
				skipped = append(skipped, fmt.Sprintf("%s (%v)", c.reg.ArchiveSessionID, err))
			}
			continue
		}
		target.describe = fmt.Sprintf("%s session %s, active %s (this machine)", c.reg.Harness.Name, c.reg.ArchiveSessionID, relativeAge(now, c.active))
		return target, true, nil
	}
	if r.source == "local" {
		message := fmt.Sprintf("no session for %s is registered on this machine with a readable transcript", dir)
		if len(skipped) > 0 {
			message += "; passed over: " + strings.Join(skipped, ", ")
		}
		return handoffTarget{}, false, errors.New(message)
	}
	return handoffTarget{}, false, nil
}

func (r handoffResolver) latestArchive(dir string, now time.Time) (handoffTarget, error) {
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return handoffTarget{}, fmt.Errorf("no local session for %s, and the archive could not be opened: %w", dir, err)
	}
	sessions, err := reader.ListMetadataWithOptions(r.ctx, store, archiveSessionsPrefix, reader.Filter{Harness: r.harness}, reader.ListOptions{Cache: listCache(r.env, false), Skipped: warnSkippedSidecar(r.stderr, "handoff")})
	if err != nil {
		return handoffTarget{}, err
	}
	sessions = topLevelSessions(sessions)
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].CapturedAt.After(sessions[j].CapturedAt) })
	projectIDs := r.projectIDs(dir)
	for _, m := range archiveHandoffCandidates(sessions, projectIDs, r.skip) {
		key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err != nil {
			continue
		}
		describe := fmt.Sprintf("%s session %s, captured %s (%s)", m.Harness.Name, m.SessionID, relativeAge(now, m.CapturedAt), r.machineLabel(m.MachineID))
		return r.fromArchive(store, key, describe)
	}
	return handoffTarget{}, r.noMatch(dir, sessions, now)
}

// archiveHandoffCandidates is the selection policy; its inputs are already
// loaded and sorted, so it can be checked without storage or a transcript.
func archiveHandoffCandidates(sessions []archive.Metadata, projectIDs, skip map[string]bool) []archive.Metadata {
	var candidates []archive.Metadata
	for _, m := range sessions {
		if projectIDs[m.ProjectID] && !skip[m.NativeSessionID] && (m.Counts.Turns == nil || *m.Counts.Turns != 0) {
			candidates = append(candidates, m)
		}
	}
	return candidates
}

// sameProject reports whether dir belongs to the project at root: it is the
// root or inside it, compared both as written and with symlinks resolved
// (macOS's /var is /private/var). A root inside dir does not count, so
// running from a parent directory such as ~ does not pick up every project
// beneath it.
func sameProject(root, dir string) bool {
	if root == "" || dir == "" {
		return false
	}
	for _, a := range pathForms(root) {
		for _, b := range pathForms(dir) {
			if local.PathWithin(b, a) {
				return true
			}
		}
	}
	return false
}

func pathForms(path string) []string {
	clean := filepath.Clean(path)
	if resolved, err := local.ResolveExistingSymlinks(clean); err == nil && resolved != clean {
		return []string{clean, filepath.Clean(resolved)}
	}
	return []string{clean}
}

// projectIDs returns the archive project IDs dir can belong to: the
// configured project whose root contains it, and dir's own ID.
func (r handoffResolver) projectIDs(dir string) map[string]bool {
	ids := map[string]bool{}
	for _, form := range pathForms(dir) {
		ids[archive.ProjectID(form)] = true
	}
	for _, project := range r.cfg.Archive.Projects {
		if project.Root != "" && sameProject(project.Root, dir) {
			ids[archive.ProjectID(project.Root)] = true
			if project.ProjectID != "" {
				ids[project.ProjectID] = true
			}
		}
	}
	return ids
}

func topLevelSessions(sessions []archive.Metadata) []archive.Metadata {
	out := sessions[:0:0]
	for _, m := range sessions {
		if m.ParentSessionID == "" {
			out = append(out, m)
		}
	}
	return out
}

func (r handoffResolver) machineLabel(machineID string) string {
	if machineID != "" && machineID == r.cfg.MachineID {
		return "this machine"
	}
	return "another machine"
}

// noMatch builds the --latest failure, listing the most recent archived
// sessions from metadata only so one can be picked directly.
func (r handoffResolver) noMatch(dir string, sessions []archive.Metadata, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no session for %s on this machine or under its project ID in the archive", dir)
	if r.harness != "" {
		fmt.Fprintf(&b, " (harness %s)", r.harness)
	}
	if len(sessions) == 0 {
		b.WriteString(".\nThe archive has no sessions yet; run `agent-archive list` after the next sync.")
		return errors.New(b.String())
	}
	b.WriteString(".\nThe project ID depends on the checkout path, so a session from another machine\nonly matches when the repository is at the same path. Recent archived sessions:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for i, m := range sessions {
		if i == handoffFallbackRows {
			break
		}
		// The table is built in memory, where writes cannot fail.
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\tagent-archive handoff %s\n", m.Harness.Name, relativeAge(now, m.CapturedAt), r.machineLabel(m.MachineID), m.SessionID)
	}
	_ = tw.Flush()
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// relativeAge renders how long ago t was, coarsely.
func relativeAge(now, t time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// handoffFullPath names where the untrimmed rendering of a bundle is saved.
func handoffFullPath(home string, bundle archive.SourceBundle, format string) string {
	ext := ".md"
	if format == "json" {
		ext = ".json"
	}
	return filepath.Join(home, handoffDir, handoffFileName(bundle)+ext)
}

// handoffFileName is the file name stem for a bundle's saved handoffs. The
// archive session ID is already a safe file component; anything else is
// hashed.
func handoffFileName(bundle archive.SourceBundle) string {
	name := bundle.ArchiveSessionID
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		sum := sha256.Sum256([]byte(bundle.ArchiveSessionID + "\x00" + bundle.NativeSessionID))
		name = hex.EncodeToString(sum[:])[:16]
	}
	return name
}

// launchHandoffPrefix begins the name of each launch copy's directory under
// handoffDir.
const launchHandoffPrefix = "launch-"

// pruneHandoffs deletes saved handoffs, and launch copies' directories,
// older than handoffMaxAge. It is best effort: a failure leaves them for the
// next run.
func pruneHandoffs(home string, now time.Time) {
	dir := filepath.Join(home, handoffDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), launchHandoffPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= handoffMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// writeHandoffOutput writes the rendered handoff with mode 0600, refusing to
// replace an existing file unless force is set.
func writeHandoffOutput(path string, data []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; pass --force to replace it", path)
		}
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
