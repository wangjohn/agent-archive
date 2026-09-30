package stats

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// unit is one session as the statistics see it: a top-level session with
// its subagent sessions rolled in. Everything the engine aggregates is
// computed here once.
type unit struct {
	root     *archive.Metadata
	children []*archive.Metadata
	harness  string
	project  string

	// day is the root's captured_at day as a day number in the window's
	// zone (see dayNumber); month is year*12 + month - 1.
	day   int
	month int

	// perModel is the unit's tokens by normalized model id, sorted by id,
	// each priced; tokens and cost are their sums. Subagents are included.
	perModel    []modelUse
	tokens      tokenSet
	hasData     bool
	approximate bool
	cost        costAcc

	// rootTokens is the root's own tokens, without its subagents.
	rootTokens  tokenSet
	rootHasData bool
	// childTokens is the subagents' tokens; childrenWithData how many
	// subagents reported any.
	childTokens        int64
	childrenWithData   int
	prompts            *int64
	toolErrors         int64
	toolResults        int64
	toolMembersKnown   int
	toolMembersUnknown int
	skills             map[string]struct{}
	mcp                map[string]int64
	lacksParser014     bool
	orphan             bool
}

type modelUse struct {
	id   string
	set  tokenSet
	cost costAcc
}

// buildUnits groups metadata into units, ordered by the root's captured_at
// and then id, so every later sum runs in one order whatever order the input
// came in.
func buildUnits(sessions []archive.Metadata, loc *time.Location, prices priceIndex) []*unit {
	metas := dedupe(sessions)
	byID := make(map[string]*archive.Metadata, len(metas))
	for _, m := range metas {
		if m.SessionID != "" {
			byID[m.SessionID] = m
		}
	}
	units := map[*archive.Metadata]*unit{}
	var ordered []*unit
	get := func(root *archive.Metadata) *unit {
		if u, ok := units[root]; ok {
			return u
		}
		u := &unit{root: root}
		units[root] = u
		ordered = append(ordered, u)
		return u
	}
	for _, m := range metas {
		root := resolveRoot(m, byID)
		u := get(root)
		if m != root {
			u.children = append(u.children, m)
		}
	}
	for _, u := range ordered {
		u.finish(loc, prices)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i].root, ordered[j].root
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.Before(b.CapturedAt)
		}
		return a.SessionID < b.SessionID
	})
	return ordered
}

// dedupe drops repeated session ids, keeping the metadata derived last, and
// returns the rest sorted by (captured_at, session id).
func dedupe(sessions []archive.Metadata) []*archive.Metadata {
	kept := make([]*archive.Metadata, 0, len(sessions))
	index := map[string]int{}
	for i := range sessions {
		m := &sessions[i]
		if m.SessionID != "" {
			if at, seen := index[m.SessionID]; seen {
				if m.MetadataDerivedAt.After(kept[at].MetadataDerivedAt) {
					kept[at] = m
				}
				continue
			}
			index[m.SessionID] = len(kept)
		}
		kept = append(kept, m)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CapturedAt.Equal(kept[j].CapturedAt) {
			return kept[i].CapturedAt.Before(kept[j].CapturedAt)
		}
		return kept[i].SessionID < kept[j].SessionID
	})
	return kept
}

// resolveRoot follows parent_session_id up to the topmost session present in
// the input: one that names no parent, or whose parent is missing (an orphan
// subagent, which then counts as a session of its own). A chain that loops
// makes the session its own root, so no session is ever counted twice.
func resolveRoot(m *archive.Metadata, byID map[string]*archive.Metadata) *archive.Metadata {
	seen := map[*archive.Metadata]bool{m: true}
	current := m
	for current.ParentSessionID != "" {
		parent, ok := byID[current.ParentSessionID]
		if !ok {
			return current
		}
		if seen[parent] {
			return m
		}
		seen[parent] = true
		current = parent
	}
	return current
}

func (u *unit) members() []*archive.Metadata {
	return append([]*archive.Metadata{u.root}, u.children...)
}

// finish computes everything derived from the unit's members.
func (u *unit) finish(loc *time.Location, prices priceIndex) {
	root := u.root
	u.harness = archive.CanonicalHarness(root.Harness.Name)
	u.project = strings.TrimSpace(root.ProjectName)
	captured := root.CapturedAt.In(loc)
	u.day = dayNumber(civilOf(captured))
	u.month = captured.Year()*12 + int(captured.Month()) - 1
	u.orphan = root.ParentSessionID != ""
	u.lacksParser014 = !parserAtLeast(root.Parser.Version, 0, 14, 0)
	if root.Counts.Turns != nil {
		turns := value(root.Counts.Turns)
		u.prompts = &turns
	}
	u.skills = map[string]struct{}{}
	u.mcp = map[string]int64{}
	merged := map[string]tokenSet{}
	for i, m := range u.members() {
		byModel, has, approximate := memberUsage(m)
		var memberTotal tokenSet
		for id, set := range byModel {
			all := merged[id]
			all.add(set)
			merged[id] = all
			memberTotal.add(set)
		}
		if has {
			u.hasData = true
			u.approximate = u.approximate || approximate
			if i == 0 {
				u.rootHasData = true
				u.rootTokens = memberTotal
			} else {
				u.childrenWithData++
				u.childTokens = satAdd(u.childTokens, memberTotal.total())
			}
		}
		u.addToolErrors(m)
		for _, skill := range m.SkillsUsed {
			if name := strings.TrimSpace(skill.Name); name != "" {
				u.skills[name] = struct{}{}
			}
		}
		for _, call := range m.MCPCalls {
			if name := strings.TrimSpace(call.Name); name != "" && call.Count > 0 {
				u.mcp[name] = satAdd(u.mcp[name], int64(call.Count))
			}
		}
	}
	for _, id := range sortedModels(merged) {
		use := modelUse{id: id, set: merged[id]}
		if usd, ok := prices.price(id, use.set); ok {
			use.cost = costAcc{usd: usd, priced: true, approximate: u.approximate}
		} else {
			use.cost = costAcc{unpriced: use.set.total(), approximate: u.approximate}
		}
		u.perModel = append(u.perModel, use)
		u.tokens.add(use.set)
		u.cost.add(use.cost)
	}
}

// addToolErrors counts a member's tool errors when it knows them: both the
// error count and the results it is out of.
func (u *unit) addToolErrors(m *archive.Metadata) {
	if m.Counts.ToolErrors == nil || m.Counts.ToolResults == nil {
		u.toolMembersUnknown++
		return
	}
	u.toolMembersKnown++
	u.toolErrors = satAdd(u.toolErrors, value(m.Counts.ToolErrors))
	u.toolResults = satAdd(u.toolResults, value(m.Counts.ToolResults))
}

// parserAtLeast reports whether a "major.minor.patch" version is at least
// the given one. A version that does not parse is older than any.
func parserAtLeast(version string, major, minor, patch int) bool {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return false
	}
	var have [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		have[i] = n
	}
	want := [3]int{major, minor, patch}
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}
