package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// archiveSessionsPrefix is the provider-relative prefix every published
// session lives under; see archive.MetadataObjectKey and
// archive.SourceObjectKey, which both produce "sessions/<harness>/<id>/...".
const archiveSessionsPrefix = "sessions"

// notSetUpMessage is what every read-only command prints, to stderr with
// exit 1 like sync and pause, when setup has never run: its output is often
// captured (claude "$(agent-archive handoff --latest)"), and must not pass
// the message off as a result. It is deliberately the same line `status`
// prints, so a first-time user gets one consistent answer.
const notSetUpMessage = "Not set up. Run `agent-archive setup` to get started."

// eligibleNoUseUnavailableMessage is what `list --skill-usage eligible_no_use`
// prints, with exit 0 and no rows. No parser version records both a complete
// eligible-skill set and complete use observation, so no sidecar carries the
// observed_none detection the query compares against and it can match nothing.
// help.go and docs/guides/list-and-show.md state the same thing.
const eligibleNoUseUnavailableMessage = "--skill-usage eligible_no_use cannot return sessions yet: no parser version\n" +
	"records both a complete eligible-skill set and complete use observation, so\n" +
	"non-use is never proven. The value stays accepted for forward compatibility."

// openReadOnlyStore loads configuration and opens the configured object
// store the same way a collector pass does (env.openStore), but without the
// machine lock or the pause check: `list` and `show` only read remote
// objects and never touch local collector state, so they may run alongside
// a scheduled `_collect` and while collection is paused. found is false,
// with a nil error, when setup has never run.
func openReadOnlyStore(env Env) (storage.ObjectStore, bool, error) {
	home, err := env.home()
	if err != nil {
		return nil, false, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return nil, false, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return nil, true, fmt.Errorf("open storage: %w", err)
	}
	return store, true, nil
}

// runListCommand implements `agent-archive list`. It reads only metadata
// sidecars (reader.ListMetadata downloads no source bundle) and prints only
// metadata fields, so its output can never contain transcript content. It
// reuses unchanged sidecars from the local metadata cache unless --no-cache.
func runListCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := newCommandFlags("list", stderr)
	harness := fs.String("harness", "", "only sessions from this harness (codex, claude, cursor)")
	model := fs.String("model", "", "only sessions that requested or observed this model")
	skill := fs.String("skill", "", "only sessions involving this skill (see --skill-usage)")
	skillSHA256 := fs.String("skill-sha256", "", "only sessions involving this exact lowercase skill SHA-256")
	skillUsage := fs.String("skill-usage", string(reader.SkillUsageUsed), "with --skill/--skill-sha256: used, available, or eligible_no_use")
	since := fs.String("since", "", "only sessions captured at or after this date (2026-01-31), RFC 3339 time, or age (7d, 12h)")
	complete := fs.Bool("complete", false, "only sessions with complete parser coverage and no capture gaps")
	noCache := fs.Bool("no-cache", false, "download every metadata sidecar instead of reusing unchanged ones from the local metadata cache")
	imported := fs.Bool("imported", false, "only sessions agent-archive backfill imported")
	hookCaptured := fs.Bool("hook-captured", false, "only sessions captured by hooks as they ran")
	jsonOut := fs.Bool("json", false, "print a versioned JSON document of the matching sessions' metadata")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if *imported && *hookCaptured {
		return fs.usageError("choose one of --imported and --hook-captured")
	}
	if *skillSHA256 != "" && !validLowerSHA256(*skillSHA256) {
		return fs.usageError("--skill-sha256 must be exactly 64 lowercase hexadecimal characters")
	}
	// The value is checked before the --skill/--skill-sha256 requirement so
	// that a misspelled value is reported as the misspelling it is, rather
	// than as a missing companion flag.
	usage := reader.SkillUsage(*skillUsage)
	switch usage {
	case reader.SkillUsageUsed, reader.SkillUsageAvailable, reader.SkillUsageEligibleNoUse:
	default:
		return fs.usageError("--skill-usage must be used, available, or eligible_no_use, not %q", *skillUsage)
	}
	if usage != reader.SkillUsageUsed && *skill == "" && *skillSHA256 == "" {
		return fs.usageError("--skill-usage requires --skill or --skill-sha256")
	}
	// No parser version records both a complete eligible-skill set and
	// complete use observation, so nothing in the bucket can carry the
	// observed_none detection this query compares against. Say so instead
	// of scanning metadata and reporting an empty result that reads like an
	// answer. The value stays accepted so scripts keep working once a
	// parser version emits that evidence.
	if usage == reader.SkillUsageEligibleNoUse {
		if *jsonOut {
			return printJSON(stdout, stderr, listDocument{Version: listSchemaVersion, Sessions: []archive.Metadata{}, Unavailable: eligibleNoUseUnavailableMessage})
		}
		fmt.Fprintln(stdout, eligibleNoUseUnavailableMessage)
		return 0
	}
	filter := reader.Filter{Harness: *harness, Model: *model, Skill: *skill, SkillSHA256: *skillSHA256, RequireCompleteCoverage: *complete, SkillUsage: usage}
	if *since != "" {
		from, err := parseSince(*since, env.now())
		if err != nil {
			return fs.usageError("--since: %v", err)
		}
		filter.From = from
	}

	store, found, err := openReadOnlyStore(env)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stderr, notSetUpMessage)
		return 1
	}
	sessions, err := reader.ListMetadataWithOptions(context.Background(), store, archiveSessionsPrefix, filter, reader.ListOptions{Cache: listCache(env, *noCache), Skipped: warnSkippedSidecar(stderr, "list")})
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	if *imported || *hookCaptured {
		kept := sessions[:0]
		for _, m := range sessions {
			if (m.Origin == archive.SessionOriginImport) == *imported {
				kept = append(kept, m)
			}
		}
		sessions = kept
	}
	if *jsonOut {
		if sessions == nil {
			sessions = []archive.Metadata{}
		}
		return printJSON(stdout, stderr, listDocument{Version: listSchemaVersion, Sessions: sessions})
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No archived sessions match.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tHARNESS\tCAPTURED\tORIGIN\tPARSER\tMODELS\tSKILLS USED")
	for _, m := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.SessionID, m.Harness.Name, formatTimeOrNever(m.CapturedAt), sessionOrigin(m), m.Parser.Status, listOrDash(modelNames(m)), listOrDash(skillNames(m)))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%d session(s).\n", len(sessions))
	return 0
}

// listSchemaVersion versions the `list --json` document.
const listSchemaVersion = 1

// listDocument is what `list --json` prints: each matching session's
// metadata sidecar, as `show` prints one, and never conversation content.
// Unavailable explains a query that cannot return sessions yet, where the
// text listing prints the same explanation instead of a table.
type listDocument struct {
	Version     int                `json:"schema_version"`
	Sessions    []archive.Metadata `json:"sessions"`
	Unavailable string             `json:"unavailable,omitempty"`
}

// warnSkippedSidecar reports, on stderr, a metadata sidecar a listing left
// out because it does not validate (damaged, or written by a newer version
// of agent-archive), so stdout keeps its format while the gap is visible.
func warnSkippedSidecar(stderr io.Writer, command string) func(reader.SkippedSidecar) {
	if stderr == nil {
		return nil
	}
	return func(s reader.SkippedSidecar) {
		fmt.Fprintf(stderr, "agent-archive: %s: warning: skipped a session whose metadata could not be read: %v\n", command, s.Err)
	}
}

// listCache opens the disposable metadata cache `list` uses to skip
// downloading sidecars whose ETag has not changed. It holds metadata only.
// The cache is an optimization, so a data directory or cache that cannot be
// opened means an uncached listing, never a failed one.
func listCache(env Env, disabled bool) *reader.MetadataCache {
	if disabled {
		return nil
	}
	home, err := env.home()
	if err != nil {
		return nil
	}
	cache, err := reader.OpenMetadataCache(home)
	if err != nil {
		return nil
	}
	return cache
}

// sessionOrigin is list's ORIGIN column: imported for a session backfill
// imported, hook for one captured as it ran.
func sessionOrigin(m archive.Metadata) string {
	if m.Origin == archive.SessionOriginImport {
		return "imported"
	}
	return "hook"
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// runShowCommand implements `agent-archive show SESSION_ID`. By
// default it prints only the session's metadata sidecar. Conversation
// content — the normalized view derived from the verified source bundle —
// is printed only when the user passes --normalized explicitly, keeping the
// spec's rule that nothing prints transcript contents unless asked.
func runShowCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := newCommandFlags("show", stderr)
	harness := fs.String("harness", "", "the session's harness, if the same ID exists under more than one")
	normalized := fs.Bool("normalized", false, "also download, verify, and print the normalized conversation view (this prints transcript content)")
	// show always prints JSON; --json is accepted so the three inspection
	// commands (list, show, status) take the same flag.
	_ = fs.Bool("json", false, "print JSON (the default and only format; accepted for consistency with list and status)")
	// Flags may follow SESSION_ID too (`show SESSION_ID --normalized`).
	sessionID, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if sessionID == "" {
		return fs.usageError("a SESSION_ID is required (see agent-archive list)")
	}

	store, found, err := openReadOnlyStore(env)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stderr, notSetUpMessage)
		return 1
	}
	ctx := context.Background()
	key, err := locateMetadataKey(ctx, store, *harness, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}

	if !*normalized {
		metadata, err := reader.ReadMetadata(ctx, store, key)
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		return printJSON(stdout, stderr, metadataWithLinks(ctx, store, metadata))
	}

	metadata, bundle, err := reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	if err != nil {
		if errors.Is(err, reader.ErrRefreshRequired) {
			fmt.Fprintf(stderr, "agent-archive: show: the session's source bundle is not available (it may have just been replaced or deleted by retention); retry, or run `agent-archive show %s` without --normalized for its metadata\n", sessionID)
		} else {
			fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		}
		return 1
	}
	view, err := archive.ParseNormalized(bundle)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: normalized view unavailable: %v\n", err)
		return 1
	}
	if code := printJSON(stdout, stderr, metadataWithLinks(ctx, store, metadata)); code != 0 {
		return code
	}
	return printJSON(stdout, stderr, normalizedOutput{Turns: view.Turns, ToolCalls: view.ToolCalls, ToolResults: view.ToolResults, HookFinals: view.HookFinals})
}

// Preserve the sidecar fields while exposing live link availability separately.
// The recorded link status is historical; retention can remove a child later.
//
// `linked_session_availability` is a CLI-only field printed beside the
// sidecar's own fields, so this object is deliberately not an instance of
// metadata.schema.json, which sets additionalProperties:false. The schema
// governs stored metadata objects; `show` renders a view of one, and keeping
// the sidecar's fields at the top level is what existing readers of this
// command already parse. Validate stored objects, not command output.
func metadataWithLinks(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata) any {
	return struct {
		archive.Metadata
		LinkedAvailability []reader.LinkedAvailability `json:"linked_session_availability,omitempty"`
	}{metadata, reader.ResolveLinkedSessions(ctx, store, metadata)}
}

// normalizedOutput is the JSON shape `show --normalized` prints for
// archive.NormalizedView, which carries no JSON tags of its own.
// NativeSkillUses is left out: it is an intermediate deriveSkills folds into
// the metadata's skills_used, which the sidecar printed first already shows.
//
// Each tool call carries the tool's name, its retained arguments, and the
// result it was linked to (record index, error flag, and retained output
// size). tool_results lists the results themselves, including any the parser
// could not safely link to a call.
type normalizedOutput struct {
	Turns       []archive.NormalizedTurn          `json:"turns"`
	ToolCalls   []archive.NormalizedToolCall      `json:"tool_calls"`
	ToolResults []archive.NormalizedToolResult    `json:"tool_results"`
	HookFinals  []archive.HookFinalReconciliation `json:"hook_finals"`
}

// locateMetadataKey resolves an archive session ID to its metadata sidecar
// key. With a harness the key is derived directly (the caller's one Get, no
// listing); without one each known harness's key is read directly, the archive
// is listed only if none exists, and the same ID published under more than
// one harness is reported as ambiguous rather than guessed.
func locateMetadataKey(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (string, error) {
	if harness != "" {
		key, err := archive.MetadataObjectKey(harness, sessionID)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	keys, err := reader.FindMetadataKeys(ctx, store, archiveSessionsPrefix, sessionID)
	if err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", fmt.Errorf("no archived session %q (see `agent-archive list`)", sessionID)
	case 1:
		return keys[0], nil
	}
	harnesses := make([]string, 0, len(keys))
	for _, key := range keys {
		harnesses = append(harnesses, strings.Split(strings.TrimPrefix(key, archiveSessionsPrefix+"/"), "/")[0])
	}
	return "", fmt.Errorf("session %q exists under more than one harness (%s); pass --harness", sessionID, strings.Join(harnesses, ", "))
}

func printJSON(stdout, stderr io.Writer, value any) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: encode output: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// parseSince is list's --since: parseTimeArg with a date taken as midnight
// UTC, as metadata capture times are recorded.
func parseSince(value string, now time.Time) (time.Time, error) {
	return parseTimeArg(value, now, time.UTC)
}

// maxAgeDays bounds an age in days (about 270 years), far inside what a
// time.Duration can hold.
const maxAgeDays = 100000

// parseTimeArg reads the time forms every command's --since (and backfill's
// --until) accepts: a calendar date (2026-01-31, midnight in loc), an RFC
// 3339 time, or an age relative to now written as a Go duration (12h, 90m)
// or in whole days (7d).
func parseTimeArg(value string, now time.Time, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, loc); err == nil {
		return t, nil
	}
	if strings.HasSuffix(value, "d") {
		// Past maxAgeDays a time.Duration would overflow.
		if days, err := strconv.Atoi(strings.TrimSuffix(value, "d")); err == nil && days >= 0 && days <= maxAgeDays {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(value); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-01-31), an RFC 3339 time, or an age (7d, 12h)", value)
}

// modelNames returns the distinct model names a session's metadata
// attributes to it, whichever side (request or response) reported them.
func modelNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, model := range m.Models {
			add(model.Attributes["gen_ai.request.model"])
			add(model.Attributes["gen_ai.response.model"])
		}
	})
}

func skillNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, skill := range m.SkillsUsed {
			add(skill.Name)
		}
	})
}

func distinctSorted(collect func(add func(string))) []string {
	seen := map[string]bool{}
	var out []string
	collect(func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	})
	sort.Strings(out)
	return out
}

func listOrDash(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}
