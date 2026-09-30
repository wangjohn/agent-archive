package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type handoffOptions struct {
	sessionID  string
	project    string
	harness    string
	file       string
	source     string
	format     string
	output     string
	to         string
	latest     bool
	force      bool
	noPreamble bool
	maxBytes   int
	// worktree launches in a new git worktree on branch (default
	// handoff/<short id>) instead of the current checkout.
	worktree bool
	branch   string
	// here and newWindow force where a launched agent runs.
	here      bool
	newWindow bool
	// agentArgs are the arguments after `--`, given to the launched agent.
	agentArgs []string
}

type handoffDestination string

const (
	handoffDestinationClaude handoffDestination = "claude"
	handoffDestinationCodex  handoffDestination = "codex"
	handoffDestinationCursor handoffDestination = "cursor"
)

func parseHandoffOptions(args []string, stderr io.Writer, env handoffOptionsDependencies, interactive bool) (handoffOptions, bool) {
	fs := env.newCommandFlags("handoff", stderr)
	latest := fs.Bool("latest", false, "the most recent session for the project")
	project := fs.String("project", "", "the project directory --latest searches (default: the current directory)")
	harness := fs.String("harness", "", "only sessions from this harness (claude, codex, cursor)")
	file := fs.String("file", "", "render this native transcript file directly (requires --harness)")
	source := fs.String("source", "auto", "where session content comes from: auto, local, or archive")
	maxBytes := fs.Int("max-bytes", archive.DefaultHandoffMaxBytes, "output budget in bytes; 0 means no limit")
	format := fs.String("format", "markdown", "markdown or json")
	output := fs.String("output", "", "write to this file (mode 0600) instead of stdout")
	force := fs.Bool("force", false, "with --output, replace an existing file")
	noPreamble := fs.Bool("no-preamble", false, "omit the note addressed to the receiving agent")
	to := fs.String("to", "", "launch a local claude, codex, or cursor session with this handoff")
	worktree := fs.Bool("worktree", false, "launch the agent in a new git worktree carrying this checkout's uncommitted and untracked files")
	branch := fs.String("branch", "", "with --worktree, the new branch (default: handoff/ and the first 8 characters of SESSION_ID)")
	here := fs.Bool("here", false, "run the launched agent in this terminal")
	newWindow := fs.Bool("new-window", false, "open the launched agent in a new terminal window or tab")
	// Everything after the first `--` belongs to the agent, not to flag
	// parsing, which would otherwise read it as a session ID.
	var agentArgs []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, agentArgs = args[:i], slices.Clone(args[i+1:])
	}
	sessionID, ok := fs.parseWithArgument(args)
	if !ok {
		return handoffOptions{}, false
	}
	usageError := func(message string) (handoffOptions, bool) {
		fs.usageError("%s", message)
		return handoffOptions{}, false
	}
	selectors := 0
	for _, set := range []bool{sessionID != "", *latest, *file != ""} {
		if set {
			selectors++
		}
	}
	switch {
	// With --to the command decides after parsing: the calling agent's own
	// session, else the picker on a terminal (runHandoffCommand).
	case selectors == 0 && !interactive && *to == "":
		return usageError(noSelectorMessage)
	case selectors > 1:
		return usageError("a session ID, --latest, and --file are mutually exclusive")
	case *file != "" && *harness == "":
		return usageError("--file requires --harness (claude, codex, or cursor)")
	case *project != "" && !*latest:
		return usageError("--project applies only to --latest")
	case *force && *output == "":
		return usageError("--force applies only to --output")
	case *maxBytes < 0:
		return usageError("--max-bytes must be 0 or more")
	case len(agentArgs) > 0 && *to == "":
		return usageError("arguments after -- go to the launched agent; name it with --to")
	case *worktree && *to == "" && !offersDestinations(handoffOptions{output: *output, format: *format, noPreamble: *noPreamble}, interactive):
		return usageError("--worktree applies to a launched agent: name it with --to, or choose one on a terminal")
	case *branch != "" && !*worktree:
		return usageError("--branch applies only to --worktree")
	}
	if message := validateHandoffLaunchOptions(*to, *output, *format, *noPreamble); message != "" {
		return usageError(message)
	}
	if message := validateHandoffWindowOptions(handoffOptions{to: *to, output: *output, format: *format, noPreamble: *noPreamble, here: *here, newWindow: *newWindow}, interactive); message != "" {
		return usageError(message)
	}
	canonical, ok := harnessFlag(*harness)
	if !ok {
		return usageError(harnessFlagError(*harness))
	}
	*harness = canonical
	switch *source {
	case "auto", "local", "archive":
	default:
		return usageError(fmt.Sprintf("--source must be auto, local, or archive, not %q", *source))
	}
	switch *format {
	case "markdown", "json":
	default:
		return usageError(fmt.Sprintf("--format must be markdown or json, not %q", *format))
	}
	if *file != "" && *source == "archive" {
		return usageError("--file reads a local transcript; --source archive does not apply")
	}
	// The ID names local files and bucket keys; only the characters archive
	// session IDs are made of are accepted, so it cannot reach outside them.
	if sessionID != "" {
		if _, err := archive.MetadataObjectKey("claude", sessionID); err != nil {
			return usageError(fmt.Sprintf("%q is not an archive session ID (see `agent-archive list`)", sessionID))
		}
	}
	return handoffOptions{sessionID: sessionID, project: *project, harness: canonical,
		file: *file, source: *source, maxBytes: *maxBytes, format: *format, to: *to,
		output: *output, latest: *latest, force: *force, noPreamble: *noPreamble, here: *here, newWindow: *newWindow, agentArgs: agentArgs,
		worktree: *worktree, branch: *branch}, true
}

// noSelectorMessage is the usage error for a handoff with nothing selected
// and no terminal to pick on.
const noSelectorMessage = "name a session ID, --latest, or --file PATH; run on a terminal to pick a session"

// noCurrentSessionMessage is noSelectorMessage for --to, which can also take
// the agent session it runs in.
const noCurrentSessionMessage = "name a session ID, --latest, or --file PATH; with none, --to hands off the Claude Code, Codex, or Cursor session it runs in, or asks on a terminal"

func validateHandoffLaunchOptions(to, output, format string, noPreamble bool) string {
	switch {
	case to != "" && handoffDestination(to) != handoffDestinationClaude && handoffDestination(to) != handoffDestinationCodex && handoffDestination(to) != handoffDestinationCursor:
		return "--to must be claude, codex, or cursor"
	case to != "" && output != "":
		return "--to and --output cannot be used together"
	case to != "" && format != "markdown":
		return "--to requires markdown format"
	case to != "" && noPreamble:
		return "--to includes the receiving-agent preamble"
	default:
		return ""
	}
}

// validateHandoffWindowOptions checks --here and --new-window, which say
// where a launched agent runs.
func validateHandoffWindowOptions(opts handoffOptions, interactive bool) string {
	switch {
	case opts.here && opts.newWindow:
		return "--here and --new-window are mutually exclusive"
	// Without --to, only the destination prompt can launch an agent.
	case (opts.here || opts.newWindow) && opts.to == "" && !offersDestinations(opts, interactive):
		return "--here and --new-window apply to a launched agent: name it with --to, or choose one on a terminal"
	case opts.here && !interactive:
		return "--here needs a terminal: stdin and stdout must both be one"
	default:
		return ""
	}
}
