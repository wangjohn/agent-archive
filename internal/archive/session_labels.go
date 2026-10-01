package archive

import "strings"

// MaxPullRequests is the most pull requests Metadata.PullRequests holds, the
// first ones a session linked. The schema's pull_requests maxItems matches it.
const MaxPullRequests = 20

// PullRequestLink is a pull request a session was linked to: Claude Code's
// pr-link record, which filter 13 keeps after checking the repository and
// number. URL is the GitHub address rebuilt from those two, and absent when
// the record carried none.
type PullRequestLink struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	URL        string `json:"url,omitempty"`
}

// Labels are what a person recognizes a session by, derived from a filtered
// bundle. Name is the title the harness gave the session (Claude Code's
// custom-title, Cursor's chat name), Title the first prompt, Branch the last
// git branch the transcript recorded, and PullRequests the pull requests it
// was linked to. Every field is empty when the transcript holds none.
type Labels struct {
	Name         string
	Title        string
	Branch       string
	PullRequests []PullRequestLink
}

// SessionLabels derives name, title, branch and pull requests from a filtered
// bundle. BuildMetadata and the handoff picker's local rows both use it, so a
// session carries the same labels before and after it is published. ok
// reports whether the transcript holds a prompt from the person (a text
// transcript is taken to); false when the bundle cannot be parsed.
func SessionLabels(bundle SourceBundle) (Labels, bool) {
	view, err := ParseNormalized(bundle)
	if err != nil {
		return Labels{}, false
	}
	hasPrompt := len(bundle.NativeText) > 0
	for _, turn := range view.Turns {
		if turn.Kind == TurnKindHumanPrompt {
			hasPrompt = true
			break
		}
	}
	return deriveLabels(bundle, view), hasPrompt
}

// deriveLabels is SessionLabels for a bundle already parsed into view, so
// BuildMetadata does not parse a transcript twice.
func deriveLabels(bundle SourceBundle, view NormalizedView) Labels {
	return Labels{
		Name:         deriveSessionName(bundle),
		Title:        deriveSessionTitle(view, bundle.NativeText),
		Branch:       deriveBranch(bundle),
		PullRequests: derivePullRequests(bundle),
	}
}

// deriveSessionName is the last custom-title a Claude Code transcript holds
// (the person can rename a session, and every name is kept), or a Cursor
// chat's name from its session record, collapsed like a title. A Claude Code
// subagent has no name of its own: its name is the description its parent
// gave the task, from the subagent-meta record filter 14 writes first. A
// custom-title, which a subagent transcript does not normally hold, comes
// later in the records and so wins over it, as a later name always does.
// "" when there is none.
func deriveSessionName(bundle SourceBundle) string {
	name := ""
	for _, record := range bundle.NativeRecords {
		rawKind, _ := record["type"].(string)
		switch {
		case claudeLabelKind(rawKind) == claudeCustomTitleType:
			if text, _ := record["customTitle"].(string); collapseSessionTitle(text) != "" {
				name = collapseSessionTitle(text)
			}
		case rawKind == subagentMetaType:
			if text, _ := record[subagentDescriptionKey].(string); collapseSessionTitle(text) != "" {
				name = collapseSessionTitle(text)
			}
		case rawKind == "session" && bundle.Capture.SourceFormat == cursorComposerFormat:
			if text, _ := record[cursorChatNameKey].(string); collapseSessionTitle(text) != "" {
				name = collapseSessionTitle(text)
			}
		}
	}
	return name
}

// deriveBranch is the last git branch the transcript recorded, as handoff
// reads it, when it has the shape git_activity requires. "" when none was
// recorded, it is malformed, or it is HEAD (a detached checkout names no
// branch).
func deriveBranch(bundle SourceBundle) string {
	branch := validBranch(recordedBranch(bundle))
	if branch == "HEAD" {
		return ""
	}
	return branch
}

// derivePullRequests lists the pull requests the transcript's pr-link records
// name, in the order they were first linked, each once, at most
// MaxPullRequests. A record whose repository or number is out of shape is
// skipped (filter 13 drops such a record, so this only matters for a bundle
// that did not come through it).
func derivePullRequests(bundle SourceBundle) []PullRequestLink {
	var links []PullRequestLink
	seen := map[PullRequestLink]bool{}
	for _, record := range bundle.NativeRecords {
		if kind, _ := record["type"].(string); claudeLabelKind(kind) != claudePRLinkType {
			continue
		}
		repository, _ := record["prRepository"].(string)
		owner, name, ok := splitRepository(repository)
		number, numberOK := claudePRNumber(record["prNumber"])
		if !ok || !numberOK {
			continue
		}
		link := PullRequestLink{Repository: repository, Number: number}
		if seen[link] {
			continue
		}
		seen[link] = true
		if url, _ := record["prUrl"].(string); url == claudePRURL(owner, name, number) {
			link.URL = url
		}
		links = append(links, link)
		if len(links) == MaxPullRequests {
			break
		}
	}
	return links
}

// DisplayTitle is what a row shows for a session: the name its harness gave
// it, else the first prompt's preview. "" when it has neither.
func DisplayTitle(m Metadata) string {
	if strings.TrimSpace(m.Name) != "" {
		return m.Name
	}
	return m.Title
}

// LatestPR is the last pull request the session was linked to, else the last
// pull request its tool calls created (git_activity's pr_created). False
// when it has neither.
func LatestPR(m Metadata) (PullRequestLink, bool) {
	if n := len(m.PullRequests); n > 0 {
		return m.PullRequests[n-1], true
	}
	for i := len(m.GitActivity) - 1; i >= 0; i-- {
		event := m.GitActivity[i]
		if event.Kind == GitEventPRCreated && event.PRNumber > 0 {
			return PullRequestLink{Repository: event.Repository, Number: event.PRNumber, URL: event.URL}, true
		}
	}
	return PullRequestLink{}, false
}
