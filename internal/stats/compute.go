// Package stats computes the numbers behind `agent-archive stats` from
// session metadata: tokens, sessions, estimated cost, agents, models,
// projects, and highlights, over a window of calendar days with the previous
// period beside it.
//
// It is pure. Compute reads no clock, file, environment or network: the
// caller passes the metadata, the time, the time zone and the price table,
// and gets a JSON-serializable Stats back, the same one for the same input
// however the input is ordered. TestStatsImportBoundary keeps it that way.
//
// Unknown is a state, never zero. A metadata field the archive could not
// record (Cursor's tokens, tool errors for Codex, every field a sidecar
// written before parser 0.14.0 lacks) stays nil all the way out, and the
// numbers built from it say how many sessions they rest on.
package stats

import (
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Compute derives Stats from session metadata. See Stats for what counts as a
// session and how the window is bounded, and Options for the knobs.
func Compute(sessions []archive.Metadata, opts Options) Stats {
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	days := opts.Days
	if days <= 0 {
		days = DefaultDays
	}
	days = min(days, MaxDays)
	topN := opts.TopN
	if topN <= 0 {
		topN = DefaultTopN
	}
	table := opts.PriceTable
	if table.Version == "" && len(table.Models) == 0 {
		table = DefaultPriceTable()
	}
	prices := table.index()

	units := buildUnits(sessions, loc, prices)
	now := opts.Now
	if now.IsZero() && len(units) > 0 {
		for _, u := range units {
			if u.root.CapturedAt.After(now) {
				now = u.root.CapturedAt
			}
		}
	}
	if now.IsZero() {
		now = time.Unix(0, 0)
	}
	today := dayNumber(civilOf(now.In(loc)))
	first := today - days + 1

	var current, previous []*unit
	for _, u := range units {
		switch {
		case u.day >= first && u.day <= today:
			current = append(current, u)
		case u.day >= first-days && u.day < first:
			previous = append(previous, u)
		}
	}

	total := &bucket{}
	perDay := make([]bucket, days)
	active := make([]bool, days)
	for _, u := range current {
		total.add(u)
		perDay[u.day-first].add(u)
		active[u.day-first] = true
	}
	prev := &bucket{}
	prevDays := map[int]bool{}
	for _, u := range previous {
		prev.add(u)
		prevDays[u.day] = true
	}

	dailySeries, peak := daily(perDay, first)
	topProjects, projectCount := projects(current, topN)
	modelRows := models(current, prices)
	var grouped *Groups
	if opts.By != GroupNone {
		grouped = groups(current, opts.By)
	}
	out := Stats{
		Window: Window{
			Days: days, Timezone: loc.String(),
			From: startOfDay(first, loc), To: startOfDay(today+1, loc),
			FirstDay: dateString(first), LastDay: dateString(today),
			PreviousFrom: startOfDay(first-days, loc), PreviousTo: startOfDay(first, loc),
		},
		Prices: PriceInfo{
			Version: table.Version, AsOf: table.AsOf, Currency: table.Currency, Sources: table.Sources,
			Notes: table.Notes, Overridden: table.Overridden,
		},
		Coverage:      coverage(current),
		Daily:         dailySeries,
		Peak:          peak,
		Overview:      overview(total, prev, active, len(prevDays), days),
		Agents:        agents(current, total),
		Models:        modelRows,
		Projects:      topProjects,
		TotalProjects: projectCount,
		Composition:   composition(total),
		Subagents:     subagentShare(current, total),
		Skills:        skills(current, topN),
		MCP:           mcpServers(current, topN),
		Highlights:    highlights(current, units, perDay, first, modelRows, now, loc),
		Groups:        grouped,
	}
	return out
}

func coverage(current []*unit) Coverage {
	cov := Coverage{Sessions: len(current), UnknownTokensByAgent: map[string]int{}}
	agentsSeen := map[string]bool{}
	for _, u := range current {
		agentsSeen[u.harness] = true
		if u.hasData {
			cov.SessionsWithTokens++
		} else {
			cov.UnknownTokensByAgent[u.harness]++
		}
		if u.lacksParser014 {
			cov.SessionsBeforeParser014++
		}
		if u.approximate {
			cov.SessionsPricedAtMainModel++
		}
		if u.toolMembersKnown == 0 {
			cov.SessionsWithoutToolErrors++
		}
		cov.SubagentSessions += len(u.children)
		if u.orphan {
			cov.OrphanSubagents++
		}
	}
	cov.Agents = len(agentsSeen)
	return cov
}

func daily(perDay []bucket, first int) ([]Day, *Peak) {
	out := make([]Day, len(perDay))
	var peak *Peak
	for i := range perDay {
		b := &perDay[i]
		date := dateString(first + i)
		tokens := b.tokenTotal()
		if b.sessions == 0 {
			zero := int64(0)
			tokens = &zero
		}
		out[i] = Day{Date: date, Sessions: b.sessions, Tokens: tokens}
		if tokens != nil && *tokens > 0 && (peak == nil || *tokens > peak.Tokens) {
			peak = &Peak{Date: date, Tokens: *tokens}
		}
	}
	return out, peak
}

func floatPtr[T int | int64](p *T) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

func measure(value, previous *float64) Measure {
	var change *float64
	if value != nil && previous != nil && *previous != 0 {
		pct := (*value - *previous) / *previous * 100
		change = &pct
	}
	return Measure{Value: value, Previous: previous, ChangePct: change}
}

func overview(cur, prev *bucket, active []bool, prevActive, days int) Overview {
	countF := func(n int) *float64 { v := float64(n); return &v }
	activeCount := 0
	for _, on := range active {
		if on {
			activeCount++
		}
	}
	current, best := streaks(active)
	costCur, costPrev := cur.cost.cost(), prev.cost.cost()
	return Overview{
		Sessions:   measure(countF(cur.sessions), countF(prev.sessions)),
		Prompts:    measure(floatPtr(cur.promptTotal()), floatPtr(prev.promptTotal())),
		Tokens:     measure(floatPtr(cur.tokenTotal()), floatPtr(prev.tokenTotal())),
		ActiveDays: measure(countF(activeCount), countF(prevActive)),
		Cost: CostMeasure{
			Measure: measure(costCur.USD, costPrev.USD),
			Partial: costCur.Partial, Approximate: costCur.Approximate, UnpricedTokens: costCur.UnpricedTokens,
		},
		DaysInWindow: days, CurrentStreak: current, BestStreak: best,
	}
}

// harnessLabels are the names the terminal shows for the agents the archive
// captures. Any other harness shows under its own name.
var harnessLabels = map[string]string{
	archive.HarnessClaude: "Claude Code",
	archive.HarnessCodex:  "Codex",
	archive.HarnessCursor: "Cursor",
}

func harnessLabel(harness string) string {
	if label, ok := harnessLabels[harness]; ok {
		return label
	}
	if harness == "" {
		return "unknown"
	}
	return harness
}

func share(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func agents(current []*unit, total *bucket) []Agent {
	byAgent := map[string]*bucket{}
	for _, u := range current {
		if byAgent[u.harness] == nil {
			byAgent[u.harness] = &bucket{}
		}
		byAgent[u.harness].add(u)
	}
	out := make([]Agent, 0, len(byAgent))
	for harness, b := range byAgent {
		var tokenShare, hitRate *float64
		if tokens := b.tokenTotal(); tokens != nil && total.dataSessions > 0 {
			s := share(*tokens, total.tokens.total())
			tokenShare = &s
		}
		if b.dataSessions > 0 {
			hitRate = cacheHitRate(&b.tokens)
		}
		out = append(out, Agent{
			Harness: harness, Label: harnessLabel(harness), Sessions: b.sessions,
			SessionsWithTokens: b.dataSessions, UnknownTokenSessions: b.sessions - b.dataSessions,
			Tokens: b.tokenTotal(), Cost: b.cost.cost(), SessionShare: share(int64(b.sessions), int64(total.sessions)),
			TokenShare: tokenShare, CacheHitRate: hitRate,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Harness < out[j].Harness
	})
	return out
}

type modelAcc struct {
	models   map[string]struct{}
	sessions int
	tokens   int64
	cost     costAcc
	priced   bool
}

func models(current []*unit, prices priceIndex) []ModelRow {
	byLabel := map[string]*modelAcc{}
	for _, u := range current {
		seen := map[string]bool{}
		for _, use := range u.perModel {
			label := prices.label(use.id)
			acc := byLabel[label]
			if acc == nil {
				acc = &modelAcc{models: map[string]struct{}{}}
				byLabel[label] = acc
			}
			acc.models[use.id] = struct{}{}
			acc.tokens += use.set.total()
			acc.cost.add(use.cost)
			_, priced := prices[use.id]
			acc.priced = acc.priced || priced
			if !seen[label] {
				seen[label] = true
				acc.sessions++
			}
		}
	}
	var pricedCost float64
	for _, label := range sortedKeys(byLabel) {
		if acc := byLabel[label]; acc.priced {
			pricedCost += acc.cost.usd
		}
	}
	out := make([]ModelRow, 0, len(byLabel))
	for _, label := range sortedKeys(byLabel) {
		acc := byLabel[label]
		ids := make([]string, 0, len(acc.models))
		for id := range acc.models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var costShare *float64
		if acc.priced && pricedCost > 0 {
			s := acc.cost.usd / pricedCost
			costShare = &s
		}
		out = append(out, ModelRow{
			Label: label, Models: ids, Sessions: acc.sessions, Tokens: acc.tokens, Priced: acc.priced,
			Cost: acc.cost.cost(), CostShare: costShare,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priced != b.Priced {
			return a.Priced
		}
		if a.Priced && a.Cost.USD != nil && b.Cost.USD != nil && *a.Cost.USD != *b.Cost.USD {
			return *a.Cost.USD > *b.Cost.USD
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Label < b.Label
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func projects(current []*unit, topN int) ([]Project, int) {
	byName := map[string]*bucket{}
	for _, u := range current {
		if byName[u.project] == nil {
			byName[u.project] = &bucket{}
		}
		byName[u.project].add(u)
	}
	out := make([]Project, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		b := byName[name]
		out = append(out, Project{Name: name, Sessions: b.sessions, Tokens: b.tokenTotal(), Cost: b.cost.cost()})
	}
	sort.SliceStable(out, func(i, j int) bool { return projectBefore(out[i], out[j]) })
	count := len(out)
	if len(out) > topN {
		out = out[:topN]
	}
	return out, count
}

// projectBefore orders projects by tokens (a project with unknown tokens
// after every known one), then sessions, then name.
func projectBefore(a, b Project) bool {
	switch {
	case a.Tokens != nil && b.Tokens == nil:
		return true
	case a.Tokens == nil && b.Tokens != nil:
		return false
	case a.Tokens != nil && *a.Tokens != *b.Tokens:
		return *a.Tokens > *b.Tokens
	}
	if a.Sessions != b.Sessions {
		return a.Sessions > b.Sessions
	}
	return a.Name < b.Name
}

func composition(total *bucket) *Composition {
	if total.dataSessions == 0 {
		return nil
	}
	t := total.tokens
	all := t.total()
	var reasoning *int64
	if t.reasoningKnown {
		known := t.reasoning
		reasoning = &known
	}
	return &Composition{
		Total:             all,
		CacheRead:         Segment{t.read, share(t.read, all)},
		CacheWrite:        Segment{t.write, share(t.write, all)},
		FreshInput:        Segment{t.fresh, share(t.fresh, all)},
		Output:            Segment{t.out, share(t.out, all)},
		ReasoningOfOutput: reasoning,
	}
}

func subagentShare(current []*unit, total *bucket) *SubagentShare {
	var tokens int64
	sessions := 0
	for _, u := range current {
		tokens += u.childTokens
		sessions += u.childrenWithData
	}
	if tokens == 0 || total.dataSessions == 0 {
		return nil
	}
	return &SubagentShare{Sessions: sessions, Tokens: tokens, Share: share(tokens, total.tokens.total())}
}

func skills(current []*unit, topN int) []Skill {
	counts := map[string]int{}
	for _, u := range current {
		for name := range u.skills {
			counts[name]++
		}
	}
	out := make([]Skill, 0, len(counts))
	for _, name := range sortedKeys(counts) {
		out = append(out, Skill{Name: name, Sessions: counts[name]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	if len(out) > topN {
		out = out[:topN]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mcpServers(current []*unit, topN int) *MCP {
	calls := map[string]int64{}
	sessions := map[string]int{}
	for _, u := range current {
		for server, n := range u.mcp {
			calls[server] += n
			sessions[server]++
		}
	}
	if len(calls) == 0 {
		return nil
	}
	servers := make([]MCPServer, 0, len(calls))
	for _, name := range sortedKeys(calls) {
		servers = append(servers, MCPServer{Name: name, Calls: calls[name], Sessions: sessions[name]})
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Calls > servers[j].Calls })
	if len(servers) > topN {
		servers = servers[:topN]
	}
	return &MCP{Scope: MCPScope, Servers: servers}
}
