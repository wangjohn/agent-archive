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

// LabelsFromAnalysis derives presentation from one already computed analysis.
func LabelsFromAnalysis(analysis Analysis) (Labels, bool) {
	view, facts := analysis.View, analysis.Facts
	hasPrompt := facts.Text
	title := facts.TextTitle
	for _, turn := range view.Turns {
		if turn.Kind == TurnKindHumanPrompt {
			hasPrompt = true
			if title == "" {
				title = collapseSessionTitle(turn.Text)
			}
		}
	}
	branch := validBranch(facts.Branch)
	if branch == "HEAD" {
		branch = ""
	}
	var links []PullRequestLink
	seen := map[PullRequestLink]bool{}
	for _, link := range facts.PullRequests {
		key := link
		key.URL = ""
		if seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, link)
		if len(links) == MaxPullRequests {
			break
		}
	}
	return Labels{Name: collapseSessionTitle(facts.Name), Title: title, Branch: branch, PullRequests: links}, hasPrompt
}
