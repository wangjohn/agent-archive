package cli

import (
	"io"
	"strings"

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
// A --file handoff has no checkout to compare with. The result is empty when
// the directory cannot be found out.
func handoffCheckout(opts handoffOptions, env handoffCheckoutDependencies) archive.HandoffCheckout {
	if opts.file != "" {
		return archive.HandoffCheckout{}
	}
	dir, err := launchDir(opts, env)
	if err != nil {
		return archive.HandoffCheckout{}
	}
	return archive.HandoffCheckout{Directories: pathForms(dir), Branch: env.gitBranch(dir)}
}

// noteBranchDifference says on stderr when the session was on another branch
// than the checkout is. It never filters: continuing a session on a different
// branch is a real use.
func noteBranchDifference(h archive.Handoff, stderr io.Writer) {
	if h.Workspace.CurrentBranch == "" {
		return
	}
	terminal.Printf(stderr, "handoff: session was on `%s`; you are on `%s`\n", archive.DisplayLine(h.Workspace.Branch), archive.DisplayLine(h.Workspace.CurrentBranch))
}

// describeRepoMatch says on stderr which session --latest chose by repository
// and how it can be told apart from the one the person meant. A session that
// matched by path already says enough by being where the work was.
//
// The transcript's own words (the branch it was on, its first prompt) are
// shown only when the person can be asked about them: without a terminal the
// reader may be a coding agent, and a repository's own configuration is what
// steered the choice.
func describeRepoMatch(target handoffTarget, h archive.Handoff, showContent bool, stderr io.Writer) {
	terminal.Println(stderr, "handoff: matched by repository (remote origin), not by path")
	facts := []string{target.machine}
	if target.projectName != "" {
		facts = append(facts, "project "+archive.DisplayLine(target.projectName))
	}
	if showContent && h.Workspace.Branch != "" {
		facts = append(facts, "branch "+archive.DisplayLine(h.Workspace.Branch))
	}
	if h.Session.StartedAt != nil {
		facts = append(facts, "started "+h.Session.StartedAt.Format("2006-01-02 15:04 UTC"))
	}
	terminal.Printf(stderr, "  %s\n", strings.Join(facts, " · "))
	if !showContent {
		return
	}
	if title, ok := firstPrompt(target.bundle); ok && title != "" {
		terminal.Printf(stderr, "  first prompt: %s\n", archive.DisplayLine(title))
	}
}

// gateRepoMatch keeps a session chosen only by repository from being used
// without the person's say. The key comes from the current directory's git
// configuration, which any repository someone else wrote controls, so a
// repository can claim another's origin, and anyone who can write to the
// archive can label a session with any key; a path match cannot be steered
// that way. On a terminal it names the session and asks, default No. Where it
// cannot ask (a pipe, or a coding agent's shell) it refuses, naming the
// command that uses the session explicitly. done is set when the command
// should exit with code.
func gateRepoMatch(target handoffTarget, h archive.Handoff, opts handoffOptions, interactive bool, answers io.Reader, stderr io.Writer) (code int, done bool) {
	if !target.byRepo {
		return 0, false
	}
	describeRepoMatch(target, h, interactive, stderr)
	if !interactive {
		terminal.Printf(stderr, "agent-archive: handoff: not using a session that matched only by repository without asking: a repository's own configuration can name another repository's origin, and this run cannot ask. If it is the one you want, run `%s`\n", explicitHandoffCommand(target, opts))
		return 1, true
	}
	yes, err := newPrompter(answers, stderr).yesNo("Hand off this session?", false)
	if err != nil || !yes {
		terminal.Println(stderr, "handoff: canceled; nothing was launched")
		return 1, true
	}
	return 0, false
}

// explicitHandoffCommand is the command that hands off target by its ID, with
// the destination, harness, and --worktree the person already named.
func explicitHandoffCommand(target handoffTarget, opts handoffOptions) string {
	words := []string{"agent-archive", "handoff", target.bundle.ArchiveSessionID}
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
