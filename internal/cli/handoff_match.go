package cli

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// handoffCheckoutDependencies is what describing the receiving checkout uses.
type handoffCheckoutDependencies interface {
	workingDirDependencies
	gitBranch(dir string) string
}

// handoffCheckout is where the receiving agent will work: the launch
// directory, as written and with symlinks resolved, and the branch it is on.
// A --file handoff has no checkout to compare with, and neither does a
// --worktree one: the agent starts in a new worktree, not in this directory,
// so what is true of this checkout would be wrong there. The result is empty
// when the directory cannot be found out.
func handoffCheckout(opts handoffOptions, env handoffCheckoutDependencies) archive.HandoffCheckout {
	if opts.file != "" || opts.worktree {
		return archive.HandoffCheckout{}
	}
	dir, err := launchDir(opts, env)
	if err != nil {
		return archive.HandoffCheckout{}
	}
	// A branch name is whatever the repository says; it is shown as one line
	// of plain text.
	return archive.HandoffCheckout{Directories: pathForms(dir), Branch: archive.DisplayLine(env.gitBranch(dir))}
}

// noteBranchDifference says on stderr when the session was on another branch
// than the checkout is. It never filters: continuing a session on a different
// branch is a real use.
func noteBranchDifference(h archive.Handoff, stderr io.Writer) {
	if h.Workspace.CurrentBranch == "" {
		return
	}
	terminal.Printf(stderr, "handoff: session was on `%s`; you are on `%s`\n", branchInNote(h.Workspace.Branch), branchInNote(h.Workspace.CurrentBranch))
}

// branchInNote is a branch name as one short line that cannot end the
// backquotes around it.
func branchInNote(branch string) string {
	return strings.ReplaceAll(cappedLine(branch, matchFieldWidth), "`", "'")
}

// wellFormedID is the only shape of archive session ID the program makes
// (local.ID: 32 lowercase hex digits). An ID read from a registration or a
// sidecar may be anything a writer put there, and is repeated to a person or
// an agent only when it has this shape.
var wellFormedID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// shownID is id for a message, or a placeholder when it is not well formed.
func shownID(id string) string {
	if wellFormedID.MatchString(id) {
		return id
	}
	return "(unusual ID)"
}

// handoffCommandFor is the command that hands off the session with id, or the
// command that finds it when id is not well formed (nothing free-form from a
// sidecar or registration is repeated).
func handoffCommandFor(id string) string {
	if wellFormedID.MatchString(id) {
		return "agent-archive handoff " + id
	}
	return "agent-archive list"
}

// matchFieldWidth is the most display columns of a session's own words (its
// project name, its first prompt) the repository-match question shows.
const matchFieldWidth = 60

// cappedLine is text as one plain line of at most limit display columns, cut
// with an ellipsis: archive.DisplayLine (no controls, escape sequences, or
// invisible format characters), then the cap. Use it for a string a bucket or
// a transcript supplied that has to be shown in a fixed space. Columns are
// counted per character: DisplayLine has removed the joiners and emoji
// presentation selectors, so a joined emoji counts as its parts.
func cappedLine(text string, limit int) string {
	// Nothing past this many bytes can be shown, and a hostile sidecar can
	// be far longer than any name. The cut falls between characters.
	if maxBytes := limit * 8; len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return ellipsize(archive.DisplayLine(text), limit)
}

// repoMatch describes a session that matched the current directory only by
// repository, to the person who has to accept it. project and title are the
// session's own words (a bucket's sidecar can put anything there): they are
// shown only where a person is asking, capped.
type repoMatch struct {
	id      string
	machine string
	started time.Time
	project string
	title   string
}

// repoMatchGate decides whether a repository-only match may be used. It is
// given the match from metadata alone, before any of the session is read; a
// non-nil error ends the search.
type repoMatchGate func(repoMatch) error

// errRepoMatchNotUsed says a repository-only match was not accepted. The gate
// has already told the person why and what to run instead.
var errRepoMatchNotUsed = errors.New("a session that matched only by repository was not used")

func (r handoffResolver) acceptRepoMatch(match repoMatch) error {
	if r.gate == nil {
		return errRepoMatchNotUsed
	}
	return r.gate(match)
}

// newRepoMatchGate keeps a session chosen only by repository from being used
// without the person's say. The key comes from the current directory's git
// configuration, which whoever wrote a repository controls, and a session's
// key from a sidecar anyone who can write to the archive controls, so a
// repository can claim another's origin. A path match cannot be steered by a
// hostile repository that way; it can be forged by a writer of the archive,
// who can compute a project ID (a hash of a path), so the archive stays
// trusted only as far as its writers are. On a terminal the gate names the
// session and asks, default No. Where it cannot ask (a pipe, or a coding
// agent's shell) it refuses, printing the machine label and the start time
// (both ours) and the command that hands off that session, only when its ID
// is well formed; otherwise how to find it.
//
// That refusal is a speed bump, not a barrier: it stops a steered agent from
// using the session by accident, and an agent can still run the command it
// prints. Nothing free-form from the session or the bucket, its ID included
// unless well formed, is printed there, since an agent is who reads it.
func newRepoMatchGate(opts handoffOptions, interactive bool, answers io.Reader, stderr io.Writer) repoMatchGate {
	return func(match repoMatch) error {
		if !interactive {
			terminal.Printf(stderr, "handoff: matched by repository (remote origin), not by path: %s\n", matchFacts(match, false))
			terminal.Printf(stderr, "agent-archive: handoff: not using it. This session matched only by repository, not by path, and this run cannot ask the user. Ask the user whether to use it; %s\n", refusalCommand(match.id, opts))
			return errRepoMatchNotUsed
		}
		terminal.Println(stderr, "handoff: matched by repository (remote origin), not by path")
		terminal.Printf(stderr, "  %s\n", matchFacts(match, true))
		yes, err := newPrompter(answers, stderr).yesNo("Hand off this session?", false)
		if err != nil || !yes {
			terminal.Println(stderr, "handoff: canceled; nothing was launched")
			return errRepoMatchNotUsed
		}
		return nil
	}
}

// matchFacts is what identifies a session to whoever is asked. The machine
// and the start time are ours; the project name and the first prompt are the
// session's and appear only with words, capped.
func matchFacts(match repoMatch, words bool) string {
	facts := []string{match.machine}
	if words {
		if project := cappedLine(match.project, matchFieldWidth); project != "" {
			facts = append(facts, "project "+project)
		}
	}
	if !match.started.IsZero() {
		facts = append(facts, "started "+match.started.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if words {
		if title := cappedLine(match.title, matchFieldWidth); title != "" {
			facts = append(facts, "first prompt: "+title)
		}
	}
	return strings.Join(facts, " · ")
}

// refusalCommand tells the person how to use the session: the command that
// hands it off by its ID, only when the ID is well formed, else how to find
// it.
func refusalCommand(id string, opts handoffOptions) string {
	if !wellFormedID.MatchString(id) {
		return "they can find it with: agent-archive list"
	}
	return "they can run: " + explicitHandoffCommand(id, opts)
}

// explicitHandoffCommand is the command that hands off a session by its ID,
// with the destination, harness, and --worktree the person already named. It
// leaves out the rest (--format, --output, --max-bytes, --branch, --project,
// --source): the person adds what they want.
func explicitHandoffCommand(id string, opts handoffOptions) string {
	words := []string{"agent-archive", "handoff", id}
	if opts.harness != "" {
		words = append(words, "--harness", opts.harness)
	}
	if opts.to != "" {
		words = append(words, "--to", opts.to)
	}
	if opts.worktree {
		words = append(words, "--worktree")
	}
	return strings.Join(words, " ")
}
