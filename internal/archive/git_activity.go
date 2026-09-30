package archive

import (
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitEventKind names one kind of git or pull request work a session did.
type GitEventKind string

// The kinds deriveGitActivity writes. A reader must accept any other
// lowercase kind, which a newer writer may add.
const (
	GitEventCommit    GitEventKind = "commit"
	GitEventPush      GitEventKind = "push"
	GitEventPRCreated GitEventKind = "pr_created"
	GitEventPRMerged  GitEventKind = "pr_merged"
)

// GitEventSource says which recognizer confirmed a GitEvent: a shell
// command's output (git, gh) or an MCP tool's result.
type GitEventSource string

// The sources deriveGitActivity writes.
const (
	GitEventSourceShell GitEventSource = "shell"
	GitEventSourceMCP   GitEventSource = "mcp"
)

// GitEvent is one commit, push, or pull request created or merged that a
// tool call made and its result confirmed. Every string is parsed and
// validated against a narrow shape, never copied from free text, and URL is
// rebuilt from the parsed parts. Commit messages, pull request text, and
// command lines are never kept.
type GitEvent struct {
	Kind GitEventKind `json:"kind"`
	// At is the timestamp of the record carrying the confirming result;
	// nil when the harness wrote none.
	At     *time.Time     `json:"at,omitempty"`
	Source GitEventSource `json:"source"`
	// SHA is the commit a commit event created, the new tip a push moved a
	// ref to, or a merge's commit, when the output reported one.
	SHA        string `json:"sha,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
	PRNumber   int    `json:"pr_number,omitempty"`
	URL        string `json:"url,omitempty"`
}

// MaxGitActivity is the most entries Metadata.GitActivity holds, the first
// ones in transcript order. The schema's git_activity maxItems matches it;
// the counts stay exact beyond it.
const MaxGitActivity = 100

// gitCounts are the uncapped totals behind Metadata.GitActivity.
type gitCounts struct {
	commits, pushes, prsCreated, prsMerged int
}

func (c *gitCounts) add(kind GitEventKind) {
	switch kind {
	case GitEventCommit:
		c.commits++
	case GitEventPush:
		c.pushes++
	case GitEventPRCreated:
		c.prsCreated++
	case GitEventPRMerged:
		c.prsMerged++
	}
}

// maxBranchLength and maxPRNumber bound what an event may carry; the schema
// matches them.
const (
	maxBranchLength = 255
	maxPRNumber     = 1 << 30
)

var (
	shaPattern        = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	repoPartPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	hostPattern       = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	gitCommitCommand  = regexp.MustCompile(`\bgit(?:\s+-[cC]\s+\S+|\s+--[\w.-]+(?:=\S+)?)*\s+commit\b`)
	gitPushCommand    = regexp.MustCompile(`\bgit(?:\s+-[cC]\s+\S+|\s+--[\w.-]+(?:=\S+)?)*\s+push\b`)
	ghPRCreateCommand = regexp.MustCompile(`\bgh\s+pr\s+create\b`)
	ghPRMergeCommand  = regexp.MustCompile(`\bgh\s+pr\s+merge\b`)
	commitLine        = regexp.MustCompile(`(?m)^\[([^\]\n]+) ([0-9a-f]{7,40})\] `)
	pushRemoteLine    = regexp.MustCompile(`^To (\S+)$`)
	pushUpdateLine    = regexp.MustCompile(`^\s*\+?\s*[0-9a-f]{7,40}\.\.\.?([0-9a-f]{7,40})\s+\S+ -> (\S+)`)
	pushNewBranchLine = regexp.MustCompile(`^\s*\*\s+\[new branch\]\s+\S+ -> (\S+)`)
	pullURLLine       = regexp.MustCompile(`(?m)^https://([A-Za-z0-9.-]+)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)\s*$`)
	pullURLAnywhere   = regexp.MustCompile(`https://([A-Za-z0-9.-]+)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)`)
	ghMergedLine      = regexp.MustCompile(`(?:Merged|Squashed and merged|Rebased and merged) pull request (?:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+))?#([0-9]+)`)
	ghRepoFlag        = regexp.MustCompile(`(?:-R|--repo)[= ]\s*(?:([A-Za-z0-9.-]+)/)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(?:\s|$)`)
	exitCodeLine      = regexp.MustCompile(`(?mi)^(?:exit code:?|process exited with code)\s+(-?[0-9]+)\s*$`)
	commandSeparator  = regexp.MustCompile(`&&|\|\||;|\n`)
	pushDryRunFlag    = regexp.MustCompile(`(?:^|\s)(?:--dry-run|-n|--porcelain)(?:\s|$)`)
	ghAutoMergeFlag   = regexp.MustCompile(`(?:^|\s)--auto(?:\s|$)`)
)

// deriveGitActivity lists the git and pull request work the session's calls
// confirmed, in transcript order, capped at MaxGitActivity, and counts all of
// it. A call counts only when a result was linked to it, the result is not
// an error, and its output shows the effect (a commit's SHA, a push's ref
// update, a pull request's URL, a merge's confirmation): an attempt that
// failed, was rejected, changed nothing, or was a dry run is not an event.
func deriveGitActivity(bundle SourceBundle, calls []NormalizedToolCall) ([]GitEvent, gitCounts) {
	var events []GitEvent
	for _, call := range calls {
		if call.ResultRecordIndex == nil || (call.IsError != nil && *call.IsError) {
			continue
		}
		name := callToolName(call)
		var found []GitEvent
		if server, tool, ok := mcpServerTool(name); ok {
			found = mcpGitEvents(server, tool, call)
		} else if command := shellCommandText(name, call); command != "" {
			found = shellGitEvents(command, call.resultText, recordBranch(bundle, call.RecordIndex))
		}
		at := recordTime(bundle, *call.ResultRecordIndex)
		for i := range found {
			found[i].At = at
		}
		events = append(events, found...)
	}
	fillMergedFromCreated(events)
	var counts gitCounts
	for _, event := range events {
		counts.add(event.Kind)
	}
	if len(events) > MaxGitActivity {
		events = events[:MaxGitActivity]
	}
	return events, counts
}

// mcpServerTool splits an MCP tool name, mcp__<server>__<tool>.
func mcpServerTool(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, mcpToolPrefix)
	if !ok {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, mcpNameSeparator)
	if !ok || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// shellCommandText is the command a shell call ran, as toolSummary reads it,
// or a Codex exec script whole (its exec_command calls are in it). "" for
// any other call.
func shellCommandText(name string, call NormalizedToolCall) string {
	lower := strings.ToLower(name)
	if shellToolNames[lower] {
		if command := argumentText(call.Input, "command", "cmd"); command != "" {
			return command
		}
		return argumentText(call.raw, "command")
	}
	if lower == "exec" {
		if script, ok := call.raw["input"].(string); ok {
			return script
		}
	}
	return ""
}

// recordBranch is the branch a Claude Code record says the session was on,
// or "" when it names none or one outside the published shape.
func recordBranch(bundle SourceBundle, index int) string {
	if index < 0 || index >= len(bundle.NativeRecords) {
		return ""
	}
	return validBranch(firstString(bundle.NativeRecords[index], "gitBranch"))
}

// recordTime is the timestamp of one native record, or nil when it has none.
func recordTime(bundle SourceBundle, index int) *time.Time {
	if index < 0 || index >= len(bundle.NativeRecords) {
		return nil
	}
	at := parseNativeTimestamp(bundle.NativeRecords[index])
	if at.IsZero() {
		return nil
	}
	return &at
}

// shellGitEvents reads the events one shell command confirmed. The command
// only gates which outputs are read; the evidence is the output, so a
// chained command (git commit … && git push) can yield several events.
func shellGitEvents(command, output, branch string) []GitEvent {
	output, ok := shellOutput(output)
	if !ok {
		return nil
	}
	var events []GitEvent
	if gitCommitCommand.MatchString(command) {
		for _, match := range commitLine.FindAllStringSubmatch(output, -1) {
			events = append(events, GitEvent{
				Kind: GitEventCommit, Source: GitEventSourceShell,
				SHA: match[2], Branch: commitBranch(match[1]),
			})
		}
	}
	if segment := commandSegment(command, gitPushCommand); segment != "" && !pushDryRunFlag.MatchString(segment) {
		events = append(events, pushEvents(output)...)
	}
	if ghPRCreateCommand.MatchString(command) {
		for _, match := range pullURLLine.FindAllStringSubmatch(output, -1) {
			if event, ok := pullRequestEvent(GitEventPRCreated, match[1], match[2], match[3], match[4]); ok {
				event.Source, event.Branch = GitEventSourceShell, branch
				events = append(events, event)
			}
		}
	}
	if segment := commandSegment(command, ghPRMergeCommand); segment != "" && !ghAutoMergeFlag.MatchString(segment) {
		events = append(events, ghMergeEvents(segment, output)...)
	}
	return events
}

// shellOutput unwraps a Codex shell result ({"output": …, "metadata":
// {"exit_code": N}}) and reports false when the output says the command
// exited non-zero, which a harness without an is_error flag records only
// there.
func shellOutput(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var wrapped struct {
			Output   *string `json:"output"`
			Metadata struct {
				ExitCode *float64 `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapped) == nil && wrapped.Output != nil {
			if wrapped.Metadata.ExitCode != nil && *wrapped.Metadata.ExitCode != 0 {
				return "", false
			}
			text = *wrapped.Output
		}
	}
	for _, match := range exitCodeLine.FindAllStringSubmatch(text, -1) {
		if match[1] != "0" {
			return "", false
		}
	}
	return text, true
}

// commandSegment returns the part of command from the first match of
// pattern to the next command separator, so a flag is read only from the
// command it belongs to. "" when pattern does not match.
func commandSegment(command string, pattern *regexp.Regexp) string {
	at := pattern.FindStringIndex(command)
	if at == nil {
		return ""
	}
	rest := command[at[0]:]
	if end := commandSeparator.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

// commitBranch reads the branch from git commit's "[<branch> <sha>]" line:
// "main", "main (root-commit)", or "detached HEAD" (no branch).
func commitBranch(label string) string {
	label = strings.TrimSuffix(label, " (root-commit)")
	if label == "detached HEAD" {
		return ""
	}
	return validBranch(label)
}

// pushEvents reads git push's report: one event per ref it updated or
// created under a "To <remote>" line. Rejected, deleted, and up-to-date
// refs are not events.
func pushEvents(output string) []GitEvent {
	var events []GitEvent
	host, repository := "", ""
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if match := pushRemoteLine.FindStringSubmatch(line); match != nil {
			host, repository = parseRemote(match[1])
			continue
		}
		sha, ref := "", ""
		if match := pushUpdateLine.FindStringSubmatch(line); match != nil {
			sha, ref = match[1], match[2]
		} else if match := pushNewBranchLine.FindStringSubmatch(line); match != nil {
			ref = match[1]
		} else {
			continue
		}
		event := GitEvent{Kind: GitEventPush, Source: GitEventSourceShell, SHA: sha, Branch: validBranch(ref), Repository: repository}
		if event.Branch != "" && repository != "" && publicHost(host) {
			event.URL = "https://" + host + "/" + repository + "/tree/" + escapeBranch(event.Branch)
		}
		events = append(events, event)
	}
	return events
}

// parseRemote reads a push remote (https://host/owner/repo.git,
// git@host:owner/repo.git, ssh://git@host/owner/repo) as a host and an
// owner/repo, from the last two path segments. A local path or a remote
// that does not parse yields neither.
func parseRemote(remote string) (host, repository string) {
	var path string
	if scheme, rest, ok := strings.Cut(remote, "://"); ok {
		// The filter rewrites a remote's userinfo as [REDACTED]@, which is
		// not a valid URL; userinfo is never kept, so drop it first.
		authority, _, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		parsed, err := url.Parse(scheme + "://" + rest)
		if err != nil {
			return "", ""
		}
		host, path = parsed.Hostname(), parsed.Path
	} else if at := strings.Index(remote, ":"); at > 0 && !strings.HasPrefix(remote, "/") && !strings.HasPrefix(remote, ".") {
		host, path = remote[:at], remote[at+1:]
		if user := strings.LastIndex(host, "@"); user >= 0 {
			host = host[user+1:]
		}
	} else {
		return "", ""
	}
	segments := strings.Split(strings.Trim(strings.TrimSuffix(strings.TrimRight(path, "/"), ".git"), "/"), "/")
	if len(segments) < 2 {
		return "", ""
	}
	owner, name := segments[len(segments)-2], segments[len(segments)-1]
	if !repoPartPattern.MatchString(owner) || !repoPartPattern.MatchString(name) {
		return "", ""
	}
	if !hostPattern.MatchString(host) {
		host = ""
	}
	return host, owner + "/" + name
}

// publicHost reports whether host is a DNS name a URL may be built on: not
// empty, not localhost, not an IP address (a local git proxy), and dotted.
func publicHost(host string) bool {
	if host == "" || !hostPattern.MatchString(host) || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
		return false
	}
	return strings.Contains(host, ".")
}

// escapeBranch path-escapes each segment of a validated branch name.
func escapeBranch(branch string) string {
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// validBranch returns branch when it has the published shape, else "".
func validBranch(branch string) string {
	if branch == "" || len(branch) > maxBranchLength || !branchPattern.MatchString(branch) {
		return ""
	}
	return branch
}

// pullRequestEvent builds a pull request event from a parsed URL's parts,
// rebuilding the URL from them. False when a part is out of shape.
func pullRequestEvent(kind GitEventKind, host, owner, name, number string) (GitEvent, bool) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > maxPRNumber || !repoPartPattern.MatchString(owner) || !repoPartPattern.MatchString(name) {
		return GitEvent{}, false
	}
	event := GitEvent{Kind: kind, Repository: owner + "/" + name, PRNumber: n}
	if publicHost(host) {
		event.URL = "https://" + host + "/" + event.Repository + "/pull/" + strconv.Itoa(n)
	}
	return event, true
}

// ghMergeEvents reads gh pr merge's confirmation. The repository comes from
// the confirmation when gh named it, else from the command's --repo flag or
// pull request URL argument; fillMergedFromCreated may supply it later.
func ghMergeEvents(segment, output string) []GitEvent {
	var events []GitEvent
	for _, match := range ghMergedLine.FindAllStringSubmatch(output, -1) {
		// gh names a pull request's repository as owner/repo; with no
		// host given it means github.com, as its --repo flag does.
		host, owner, name := "github.com", match[1], match[2]
		if owner == "" {
			if flag := ghRepoFlag.FindStringSubmatch(segment); flag != nil {
				host, owner, name = flag[1], flag[2], flag[3]
				if host == "" {
					host = "github.com"
				}
			} else if pr := pullURLAnywhere.FindStringSubmatch(segment); pr != nil {
				host, owner, name = pr[1], pr[2], pr[3]
			}
		}
		if owner == "" {
			n, err := strconv.Atoi(match[3])
			if err == nil && n >= 1 && n <= maxPRNumber {
				events = append(events, GitEvent{Kind: GitEventPRMerged, Source: GitEventSourceShell, PRNumber: n})
			}
			continue
		}
		if event, ok := pullRequestEvent(GitEventPRMerged, host, owner, name, match[3]); ok {
			event.Source = GitEventSourceShell
			events = append(events, event)
		}
	}
	return events
}

// mcpGitEvents reads a GitHub MCP tool's result: create_pull_request,
// merge_pull_request, and the tools that commit on the server (push_files,
// create_or_update_file). Other tools, including enable_pr_auto_merge,
// yield nothing.
func mcpGitEvents(server, tool string, call NormalizedToolCall) []GitEvent {
	result := decodeObject(call.resultText)
	owner, name := firstString(call.Input, "owner"), firstString(call.Input, "repo")
	githubServer := strings.Contains(strings.ToLower(server), "github")
	switch tool {
	case "create_pull_request":
		event, ok := mcpPullRequest(GitEventPRCreated, call.resultText, result, owner, name, numberText(result["number"]), githubServer)
		if !ok {
			return nil
		}
		event.Branch = validBranch(firstString(call.Input, "head"))
		return []GitEvent{event}
	case "merge_pull_request":
		// The merged flag decides when the result has one; a text result
		// must say the merge succeeded.
		merged, flagged := result["merged"].(bool)
		if !flagged {
			merged = strings.Contains(strings.ToLower(call.resultText), "successfully merged")
		}
		if !merged {
			return nil
		}
		event, ok := mcpPullRequest(GitEventPRMerged, "", nil, owner, name, numberText(call.Input["pullNumber"]), githubServer)
		if !ok {
			return nil
		}
		if sha := firstString(result, "sha"); shaPattern.MatchString(sha) {
			event.SHA = sha
		}
		return []GitEvent{event}
	case "push_files", "create_or_update_file":
		sha := nestedString(result, "commit", "sha")
		if sha == "" {
			sha = nestedString(result, "object", "sha")
		}
		if !shaPattern.MatchString(sha) {
			return nil
		}
		event := GitEvent{Kind: GitEventCommit, Source: GitEventSourceMCP, SHA: sha, Branch: validBranch(firstString(call.Input, "branch"))}
		if repoPartPattern.MatchString(owner) && repoPartPattern.MatchString(name) {
			event.Repository = owner + "/" + name
		}
		return []GitEvent{event}
	}
	return nil
}

// mcpPullRequest builds a pull request event from a pull request URL in the
// result, or else from a number and the call's owner and repo; a URL is
// then built only for a GitHub server.
func mcpPullRequest(kind GitEventKind, text string, result map[string]any, owner, name, number string, githubServer bool) (GitEvent, bool) {
	for _, candidate := range []string{firstString(result, "html_url"), firstString(result, "url"), text} {
		if match := pullURLAnywhere.FindStringSubmatch(candidate); match != nil {
			event, ok := pullRequestEvent(kind, match[1], match[2], match[3], match[4])
			event.Source = GitEventSourceMCP
			return event, ok
		}
	}
	host := ""
	if githubServer {
		host = "github.com"
	}
	event, ok := pullRequestEvent(kind, host, owner, name, number)
	event.Source = GitEventSourceMCP
	return event, ok
}

// fillMergedFromCreated gives a merge event that could not name its
// repository the repository and URL of the pull request the session created
// with the same number, when exactly one did.
func fillMergedFromCreated(events []GitEvent) {
	for i := range events {
		if events[i].Kind != GitEventPRMerged || events[i].Repository != "" {
			continue
		}
		var source *GitEvent
		for j := range events {
			if events[j].Kind == GitEventPRCreated && events[j].PRNumber == events[i].PRNumber && events[j].Repository != "" {
				if source != nil && source.Repository != events[j].Repository {
					source = nil
					break
				}
				source = &events[j]
			}
		}
		if source != nil {
			events[i].Repository, events[i].URL = source.Repository, source.URL
		}
	}
}

// decodeObject decodes text as a JSON object, or returns nil.
func decodeObject(text string) map[string]any {
	var out map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &out) != nil {
		return nil
	}
	return out
}

// nestedString returns object[outer][inner] when it is a string.
func nestedString(object map[string]any, outer, inner string) string {
	child, _ := object[outer].(map[string]any)
	value, _ := child[inner].(string)
	return value
}

// numberText renders a JSON number or numeric string argument as decimal
// text, or "" for anything else.
func numberText(value any) string {
	switch v := value.(type) {
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
	case string:
		return v
	}
	return ""
}
