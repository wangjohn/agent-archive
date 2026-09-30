package stats

import (
	"sort"
	"strings"
)

// NoteKind names what a heads-up note is about. The kinds are listed in
// priority order: when more than MaxHeadsUp apply, the later ones are left
// out.
type NoteKind string

// The heads-up note kinds, in priority order.
const (
	// NoteSubagentShare says subagents used SubagentShareThreshold or more of
	// the window's tokens. It carries Share, Tokens and Runs.
	NoteSubagentShare NoteKind = "subagent_share"
	// NoteCostliestSession says one session cost CostliestNoteMinShare or more
	// of the window's priced spend, and more than CostliestNoteMinCost. It
	// carries Cost, CostShare, Project, Subagents and Drivers; the session
	// itself is Highlights.CostliestSession.
	NoteCostliestSession NoteKind = "costliest_session"
	// NoteUnmeteredSessions says some sessions report no token counts. It
	// carries Sessions and ByAgent.
	NoteUnmeteredSessions NoteKind = "unmetered_sessions"
	// NoteLowCacheHit says the window's cache-hit rate is below
	// LowCacheHitRate, over at least LowCacheMinInputTokens input-side tokens.
	// It carries HitRate and InputTokens.
	NoteLowCacheHit NoteKind = "low_cache_hit"
)

// Note is one heads-up: a fact the reader should not miss, as data. The
// renderer words it. Kind says which fields are set; the others are absent
// from the JSON.
type Note struct {
	Kind NoteKind `json:"kind"`

	// Share is the part (0 to 1) of the window's tokens the subagents used
	// (NoteSubagentShare).
	Share *float64 `json:"share,omitempty"`
	// Tokens is the tokens subagents used (NoteSubagentShare).
	Tokens *int64 `json:"tokens,omitempty"`
	// Runs is the number of subagent runs rolled into their parent sessions
	// in the window (Coverage.SubagentSessions), whether or not each reported
	// tokens. A run is never a session of its own (NoteSubagentShare).
	Runs *int `json:"runs,omitempty"`

	// Cost is the costliest session's estimated cost, with its partial and
	// approximate flags (NoteCostliestSession).
	Cost *Cost `json:"cost,omitempty"`
	// CostShare is that cost over the window's priced spend, 0 to 1
	// (NoteCostliestSession).
	CostShare *float64 `json:"cost_share,omitempty"`
	// Project is the costliest session's project name; empty when it has none
	// (NoteCostliestSession).
	Project string `json:"project,omitempty"`
	// Subagents is how many subagent runs the costliest session had, and
	// Drivers what likely made it costly (the Driver constants), as in
	// Highlights.CostliestSession (NoteCostliestSession).
	Subagents *int     `json:"subagents,omitempty"`
	Drivers   []string `json:"drivers,omitempty"`

	// Sessions is how many sessions report no token counts, and ByAgent the
	// same by agent, most sessions first (NoteUnmeteredSessions).
	Sessions *int            `json:"sessions,omitempty"`
	ByAgent  []AgentSessions `json:"by_agent,omitempty"`

	// HitRate is the window's cache-hit rate (cache reads over input-side
	// tokens) and InputTokens the input-side tokens it rests on
	// (NoteLowCacheHit).
	HitRate     *float64 `json:"hit_rate,omitempty"`
	InputTokens *int64   `json:"input_tokens,omitempty"`
}

// AgentSessions is a number of sessions of one agent.
type AgentSessions struct {
	Harness  string `json:"harness"`
	Label    string `json:"label"`
	Sessions int    `json:"sessions"`
}

// headsUp is the notes that apply to the window, in priority order, at most
// MaxHeadsUp of them.
func headsUp(cov Coverage, total *bucket, sub *SubagentShare, costliest *CostliestSession) []Note {
	notes := []Note{}
	if n := subagentNote(cov, sub); n != nil {
		notes = append(notes, *n)
	}
	if n := costliestNote(total, costliest); n != nil {
		notes = append(notes, *n)
	}
	if n := unmeteredNote(cov); n != nil {
		notes = append(notes, *n)
	}
	if n := lowCacheNote(total); n != nil {
		notes = append(notes, *n)
	}
	if len(notes) > MaxHeadsUp {
		notes = notes[:MaxHeadsUp]
	}
	return notes
}

func subagentNote(cov Coverage, sub *SubagentShare) *Note {
	if sub == nil || sub.Share < SubagentShareThreshold {
		return nil
	}
	share, tokens, runs := sub.Share, sub.Tokens, cov.SubagentSessions
	return &Note{Kind: NoteSubagentShare, Share: &share, Tokens: &tokens, Runs: &runs}
}

func costliestNote(total *bucket, best *CostliestSession) *Note {
	if best == nil || best.Cost.USD == nil || !total.cost.priced || total.cost.usd <= 0 {
		return nil
	}
	cost := *best.Cost.USD
	costShare := cost / total.cost.usd
	if cost <= CostliestNoteMinCost || costShare < CostliestNoteMinShare {
		return nil
	}
	c, subagents := best.Cost, best.Subagents
	return &Note{
		Kind: NoteCostliestSession, Cost: &c, CostShare: &costShare, Project: best.Project,
		Subagents: &subagents, Drivers: append([]string(nil), best.Drivers...),
	}
}

func unmeteredNote(cov Coverage) *Note {
	unknown := cov.Sessions - cov.SessionsWithTokens
	if unknown <= 0 {
		return nil
	}
	byAgent := make([]AgentSessions, 0, len(cov.UnknownTokensByAgent))
	for harness, n := range cov.UnknownTokensByAgent {
		if n > 0 {
			byAgent = append(byAgent, AgentSessions{Harness: harness, Label: harnessLabel(harness), Sessions: n})
		}
	}
	sort.Slice(byAgent, func(i, j int) bool {
		if byAgent[i].Sessions != byAgent[j].Sessions {
			return byAgent[i].Sessions > byAgent[j].Sessions
		}
		return byAgent[i].Harness < byAgent[j].Harness
	})
	return &Note{Kind: NoteUnmeteredSessions, Sessions: &unknown, ByAgent: byAgent}
}

func lowCacheNote(total *bucket) *Note {
	rate := cacheHitRate(&total.tokens)
	side := total.tokens.inputSide()
	if rate == nil || side < LowCacheMinInputTokens || *rate >= LowCacheHitRate {
		return nil
	}
	return &Note{Kind: NoteLowCacheHit, HitRate: rate, InputTokens: &side}
}

// pluginSeparator ends a plugin's name in a skill it provides
// ("plugin:skill").
const pluginSeparator = ":"

// SkillDisplayName is the name to show for a skill: the recorded name without
// its plugin prefix ("anthropic-skills:docs" shows as "docs"). Only the first
// separator counts, so "plugin:group:skill" shows as "group:skill". A name
// with nothing before or after the separator is shown as it is. The data
// keeps the recorded name: this is for display only.
func SkillDisplayName(name string) string {
	prefix, rest, found := strings.Cut(name, pluginSeparator)
	if !found || prefix == "" || rest == "" {
		return name
	}
	return rest
}
