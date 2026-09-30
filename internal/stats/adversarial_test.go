package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// hostileArchive is an archive with everything a corrupt or hostile bucket
// could hold: parent cycles and self-parents, repeated ids, a zero or far-off
// captured_at, sessions after Now, and token counts at the parser's 2^53 cap.
func hostileArchive(rng *rand.Rand, n int) []archive.Metadata {
	const maxCount = 1 << 53
	pick := func(vals ...int) int { return vals[rng.IntN(len(vals))] }
	count := func() int { return pick(0, 1, 7, 5000, maxCount, maxCount-1, rng.IntN(100000)) }
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "gpt-6-luna", "other", "unknown", "", "x", "<synthetic>", "OPUS[1m]"}
	base := time.Date(2026, time.September, 15, 12, 0, 0, 0, newYork)
	out := make([]archive.Metadata, 0, n)
	for i := range n {
		var at time.Time
		switch rng.IntN(10) {
		case 0:
			// captured_at unset
		case 1:
			at = base.AddDate(0, 0, 40+rng.IntN(400)) // after Now
		case 2:
			at = base.AddDate(-5, 0, 0)
		default:
			at = base.Add(-time.Duration(rng.IntN(60*24*60)) * time.Minute)
		}
		harness := []string{"claude", "codex", "cursor", "", "Claude Code"}[rng.IntN(5)]
		m := meta(fmt.Sprintf("h%03d", rng.IntN(n)), harness, at, project([]string{"a", "b", " a ", ""}[rng.IntN(4)]))
		m.MetadataDerivedAt = base.Add(time.Duration(i) * time.Second)
		switch rng.IntN(4) {
		case 0:
			m.ParentSessionID = fmt.Sprintf("h%03d", rng.IntN(n)) // may cycle or point at itself
		case 1:
			m.ParentSessionID = "nowhere"
		}
		if rng.IntN(3) > 0 {
			m.Counts.Turns = ip(rng.IntN(20))
		}
		if rng.IntN(3) == 0 {
			m.Counts.ToolResults, m.Counts.ToolErrors = ip(count()), ip(count())
		}
		switch rng.IntN(4) {
		case 0:
			// no tokens
		case 1:
			usedModel(models[rng.IntN(len(models))], 1)(&m)
			m.Parser.Version = []string{"0.13.0", "", "garbage", "0.14.0"}[rng.IntN(4)]
			in, out, read, write := count(), count(), count(), count()
			m.Counts.InputTokens, m.Counts.OutputTokens = ip(in), ip(out)
			m.Counts.CacheReadTokens, m.Counts.CacheWriteTokens = ip(read), ip(write)
			m.Counts.ReasoningTokens = ip(count())
		default:
			for range 1 + rng.IntN(32) {
				m.ModelTokens = append(m.ModelTokens, archive.ModelTokens{
					Model: models[rng.IntN(len(models))], InputTokens: ip(count()), OutputTokens: ip(count()),
					CacheReadTokens: ip(count()), CacheWriteTokens: ip(count()), ReasoningTokens: ip(count()),
				})
			}
		}
		out = append(out, m)
	}
	return out
}

// The property the numbers exist for, on the nastiest input: every breakdown
// adds up to the overall total, nothing is negative or non-finite, and the
// document is the same for any ordering of the input.
func TestHostileArchivesStayConsistent(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewPCG(seed, 99))
		sessions := hostileArchive(rng, 30+rng.IntN(300))
		for _, by := range []Grouping{GroupNone, GroupDay, GroupWeek, GroupMonth, GroupProject} {
			o := Options{Now: time.Date(2026, time.September, 15, 12, 0, 0, 0, newYork), Location: newYork, Days: 90, TopN: 1000, By: by}
			got := Compute(sessions, o)
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("seed %d by %q: not serializable: %v", seed, by, err)
			}
			shuffled := append([]archive.Metadata(nil), sessions...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			// Repeated ids keep the latest derivation; derived_at is distinct per
			// session here, so the choice cannot depend on the order.
			again, err := json.Marshal(Compute(shuffled, o))
			if err != nil || string(again) != string(data) {
				t.Fatalf("seed %d by %q: input order changed the document (err %v)", seed, by, err)
			}

			var total int64
			if got.Composition != nil {
				total = got.Composition.Total
			}
			var agents, models, projects, daily, groups int64
			sessionsByAgent, sessionsByProject, groupSessions := 0, 0, 0
			for _, a := range got.Agents {
				agents = satAdd(agents, sumTokens(a.Tokens))
				sessionsByAgent += a.Sessions
			}
			for _, r := range got.Models {
				models = satAdd(models, r.Tokens)
			}
			for _, p := range got.Projects {
				projects = satAdd(projects, sumTokens(p.Tokens))
				sessionsByProject += p.Sessions
			}
			for _, d := range got.Daily {
				daily = satAdd(daily, sumTokens(d.Tokens))
			}
			if got.Groups != nil {
				for _, row := range got.Groups.Rows {
					groups = satAdd(groups, sumTokens(row.Tokens))
					groupSessions += row.Sessions
				}
			}
			for name, sum := range map[string]int64{"agents": agents, "models": models, "projects": projects, "daily": daily} {
				if sum != total {
					t.Errorf("seed %d by %q: %s tokens %d != overall %d", seed, by, name, sum, total)
				}
			}
			if got.Groups != nil && (groups != total || groupSessions != got.Coverage.Sessions) {
				t.Errorf("seed %d by %q: groups tokens %d sessions %d, want %d and %d", seed, by, groups, groupSessions, total, got.Coverage.Sessions)
			}
			if sessionsByAgent != got.Coverage.Sessions || sessionsByProject != got.Coverage.Sessions {
				t.Errorf("seed %d: sessions by agent %d, by project %d, want %d", seed, sessionsByAgent, sessionsByProject, got.Coverage.Sessions)
			}
			if total < 0 {
				t.Errorf("seed %d: negative total %d", seed, total)
			}
			if c := got.Composition; c != nil {
				for name, seg := range map[string]Segment{"read": c.CacheRead, "write": c.CacheWrite, "fresh": c.FreshInput, "out": c.Output} {
					if seg.Tokens < 0 || seg.Share < 0 || seg.Share > 1.0000001 || math.IsNaN(seg.Share) {
						t.Errorf("seed %d: composition %s = %+v", seed, name, seg)
					}
				}
			}
		}
	}
}

// The archive caps each stored count at 2^53. Summed over enough sessions
// (or the 32 models of one), that passes int64, and a wrapped total would be
// negative: the sum stops at the largest int64 instead.
func TestHugeTokenCountsAddUpWithoutWrapping(t *testing.T) {
	t.Parallel()
	const maxCount = 1 << 53
	build := func(n int) []archive.Metadata {
		var sessions []archive.Metadata
		for i := range n {
			sessions = append(sessions, meta(fmt.Sprintf("s%04d", i), "claude", day(time.September, 10, 9),
				tokens(maxCount, maxCount, maxCount, maxCount), usedModel("claude-opus-5-5", 1)))
		}
		return sessions
	}
	o := Options{Now: day(time.September, 20, 12), Location: newYork}

	// 100 sessions: 400 * 2^53 fits, and every figure is exact.
	got := Compute(build(100), o)
	if want := int64(400 * maxCount); got.Composition.Total != want || *got.Agents[0].Tokens != want || got.Models[0].Tokens != want {
		t.Errorf("100 sessions: total %d, agent %s, model %d, want %d", got.Composition.Total, i64(got.Agents[0].Tokens), got.Models[0].Tokens, want)
	}

	// 300 sessions: 1200 * 2^53 does not fit; every figure saturates together.
	got = Compute(build(300), o)
	if got.Composition.Total != math.MaxInt64 || *got.Agents[0].Tokens != math.MaxInt64 || got.Models[0].Tokens != math.MaxInt64 {
		t.Errorf("300 sessions: total %d, agent %s, model %d, want %d", got.Composition.Total, i64(got.Agents[0].Tokens), got.Models[0].Tokens, int64(math.MaxInt64))
	}
	segments := map[string]Segment{
		"read": got.Composition.CacheRead, "write": got.Composition.CacheWrite,
		"fresh": got.Composition.FreshInput, "output": got.Composition.Output,
	}
	for name, seg := range segments {
		if seg.Tokens < 0 || seg.Share < 0 || seg.Share > 1 {
			t.Errorf("300 sessions: %s segment %+v", name, seg)
		}
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("saturated stats do not serialize: %v", err)
	}
}

// A negative count (the parser never writes one) is not a count: it must not
// cancel real tokens out of a total.
func TestNegativeCountsAreNotTokens(t *testing.T) {
	t.Parallel()
	m := meta("neg", "claude", day(time.September, 10, 9), usedModel("claude-opus-5-5", 1), tokens(-5, 10, 0, 0))
	m.Counts.Turns = ip(-3)
	got := Compute([]archive.Metadata{m}, Options{Now: day(time.September, 20, 12), Location: newYork})
	if got.Composition == nil || got.Composition.Total != 10 {
		t.Errorf("composition %+v, want a total of 10", got.Composition)
	}
	if p := got.Overview.Prompts.Value; p == nil || *p != 0 {
		t.Errorf("prompts %v, want 0", f64(p))
	}
}
