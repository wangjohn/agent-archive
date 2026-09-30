package stats

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func noteKinds(notes []Note) []NoteKind {
	kinds := []NoteKind{}
	for _, n := range notes {
		kinds = append(kinds, n.Kind)
	}
	return kinds
}

func findNote(tb testing.TB, notes []Note, kind NoteKind) Note {
	tb.Helper()
	for _, n := range notes {
		if n.Kind == kind {
			return n
		}
	}
	tb.Fatalf("no %s note in %v", kind, noteKinds(notes))
	return Note{}
}

// spread is n sessions of one million priced tokens each (n currency units)
// on separate days, so none is the costliest by a wide margin unless the test
// says so.
func spread(prefix string, n int) []archive.Metadata {
	out := make([]archive.Metadata, 0, n)
	for i := range n {
		out = append(out, meta(fmt.Sprintf("%s%02d", prefix, i), "claude", day(time.September, 1+i%20, 9), million(1)))
	}
	return out
}

// Each kind appears exactly at its threshold: subagents at a quarter of the
// tokens, the costliest session at a tenth of the spend and over 1 currency
// unit, a low cache-hit rate under 60% of at least 50,000 input-side tokens,
// and any session without token data.
func TestHeadsUpThresholds(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	cases := []struct {
		name     string
		sessions []archive.Metadata
		want     []NoteKind
	}{
		{"an empty archive has nothing to say", nil, []NoteKind{}},
		{"subagents at exactly a quarter of the tokens", []archive.Metadata{
			meta("p", "claude", at, modelTokens("m", 3000, 0, 0, 0)),
			meta("k", "claude", at, parentOf("p"), modelTokens("m", 1000, 0, 0, 0)),
		}, []NoteKind{NoteSubagentShare}},
		{"subagents just under a quarter", []archive.Metadata{
			meta("p", "claude", at, modelTokens("m", 3001, 0, 0, 0)),
			meta("k", "claude", at, parentOf("p"), modelTokens("m", 1000, 0, 0, 0)),
		}, []NoteKind{}},
		{"subagents without token counts have no share", []archive.Metadata{
			meta("p", "claude", at, modelTokens("m", 3000, 0, 0, 0)),
			meta("k", "claude", at, parentOf("p")),
		}, []NoteKind{}},
		{"a session costing exactly 1 is not worth a note", []archive.Metadata{meta("a", "claude", at, million(1))}, []NoteKind{}},
		{"a session costing a little over 1 and all the spend", []archive.Metadata{
			meta("a", "claude", at, modelTokens("m", 0, 0, 1_000_001, 0)),
		}, []NoteKind{NoteCostliestSession}},
		{"a session at exactly a tenth of the spend", append(spread("o", 54), meta("big", "claude", at, million(6))),
			[]NoteKind{NoteCostliestSession}},
		{"a session just under a tenth of the spend", append(spread("o", 55), meta("big", "claude", at, million(6))),
			[]NoteKind{}},
		{"one session without token counts", []archive.Metadata{meta("a", "cursor", at)}, []NoteKind{NoteUnmeteredSessions}},
		{"every session has token counts", []archive.Metadata{meta("a", "claude", at, modelTokens("m", 10, 0, 0, 0))}, []NoteKind{}},
		{"a cache-hit rate of 59% over 50,000 tokens", []archive.Metadata{
			meta("a", "claude", at, modelTokens("m", 20_500, 0, 29_500, 0)),
		}, []NoteKind{NoteLowCacheHit}},
		{"a cache-hit rate of exactly 60%", []archive.Metadata{
			meta("a", "claude", at, modelTokens("m", 20_000, 0, 30_000, 0)),
		}, []NoteKind{}},
		{"a low rate over just under 50,000 tokens says nothing", []archive.Metadata{
			meta("a", "claude", at, modelTokens("m", 49_000, 0, 999, 0)),
		}, []NoteKind{}},
		{"a low rate over exactly 50,000 tokens", []archive.Metadata{
			meta("a", "claude", at, modelTokens("m", 49_000, 0, 1_000, 0)),
		}, []NoteKind{NoteLowCacheHit}},
		{"no cache counts recorded is not a low hit rate", []archive.Metadata{
			meta("a", "claude", at, func(m *archive.Metadata) { m.Counts.InputTokens = ip(90_000) }),
		}, []NoteKind{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := noteKinds(Compute(tc.sessions, flatOptions()).HeadsUp)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("notes = %v, want %v", got, tc.want)
			}
		})
	}
}

// The notes carry what a renderer needs, and only that.
func TestHeadsUpCarriesItsData(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	sessions := []archive.Metadata{
		meta("p", "claude", at, project("styleprofile"), messages(2), compactions(1), million(2)),
		meta("k1", "claude", at, parentOf("p"), million(1)),
		meta("k2", "claude", at, parentOf("p"), million(1)),
		meta("k3", "claude", at, parentOf("p")), // a run that reported no tokens
		meta("c1", "cursor", at),
		meta("c2", "cursor", at),
		meta("c3", "cursor", at),
		meta("x1", "claude", at),
		meta("x2", "codex", at),
	}
	got := Compute(sessions, flatOptions())

	sub := findNote(t, got.HeadsUp, NoteSubagentShare)
	if sub.Share == nil || !near(*sub.Share, 0.5) || sub.Tokens == nil || *sub.Tokens != 2_000_000 || sub.Runs == nil || *sub.Runs != 3 {
		t.Errorf("subagent note = share %v tokens %s runs %v, want 0.5, 2000000 and all 3 runs", f64(sub.Share), i64(sub.Tokens), sub.Runs)
	}

	costly := findNote(t, got.HeadsUp, NoteCostliestSession)
	if costly.Cost == nil || costly.Cost.USD == nil || !near(*costly.Cost.USD, 4) || costly.CostShare == nil || !near(*costly.CostShare, 1) {
		t.Errorf("costliest note = %+v", costly)
	}
	if costly.Project != "styleprofile" || costly.Subagents == nil || *costly.Subagents != 3 ||
		!slices.Equal(costly.Drivers, []string{DriverLongContext, DriverSubagents}) {
		t.Errorf("costliest note = project %q subagents %v drivers %v", costly.Project, costly.Subagents, costly.Drivers)
	}

	unmetered := findNote(t, got.HeadsUp, NoteUnmeteredSessions)
	want := []AgentSessions{{"cursor", "Cursor", 3}, {"claude", "Claude Code", 1}, {"codex", "Codex", 1}}
	if unmetered.Sessions == nil || *unmetered.Sessions != 5 || !slices.Equal(unmetered.ByAgent, want) {
		t.Errorf("unmetered note = sessions %v by agent %+v, want 5 and %+v", unmetered.Sessions, unmetered.ByAgent, want)
	}
	// Fields of other kinds are absent, so a consumer switches on kind.
	for _, n := range got.HeadsUp {
		data, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		if n.Kind == NoteUnmeteredSessions && (strings.Contains(string(data), `"share"`) || strings.Contains(string(data), `"cost"`)) {
			t.Errorf("the unmetered note carries another kind's fields: %s", data)
		}
	}
}

func TestHeadsUpLowCacheCarriesTheRate(t *testing.T) {
	t.Parallel()
	got := Compute([]archive.Metadata{
		meta("a", "claude", day(time.September, 20, 10), modelTokens("m", 60_000, 5, 40_000, 0)),
	}, flatOptions())
	n := findNote(t, got.HeadsUp, NoteLowCacheHit)
	if n.HitRate == nil || !near(*n.HitRate, 0.4) || n.InputTokens == nil || *n.InputTokens != 100_000 {
		t.Fatalf("note = rate %v input %s", f64(n.HitRate), i64(n.InputTokens))
	}
}

// The costliest note needs a priced session; the unit is the price table's
// currency, so a table in cents crosses the floor sooner.
func TestHeadsUpCostliestNeedsPricedSpend(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	unpriced := Compute([]archive.Metadata{meta("a", "claude", at, modelTokens("x", 0, 0, 5_000_000, 0))}, flatOptions())
	if got := noteKinds(unpriced.HeadsUp); len(got) != 0 {
		t.Errorf("an unpriced model has no costliest session, got %v", got)
	}
	// A priced session that is a tenth of the spend by a large unpriced one
	// does not count the unpriced tokens as spend: the share is of priced spend.
	mixed := Compute([]archive.Metadata{
		meta("a", "claude", at, million(2), modelTokens("x", 0, 0, 500_000_000, 0)),
	}, flatOptions())
	if got := noteKinds(mixed.HeadsUp); !slices.Equal(got, []NoteKind{NoteCostliestSession}) {
		t.Errorf("mixed session: notes = %v", got)
	}
	if n := findNote(t, mixed.HeadsUp, NoteCostliestSession); n.Cost == nil || !n.Cost.Partial || n.Cost.UnpricedTokens != 500_000_000 {
		t.Errorf("the note keeps the cost's partial flag: %+v", n.Cost)
	}
}

// Every note applies at once: the three highest priority ones stay, in order,
// and the cache note is the one left out. Take the subagent note away and it
// comes back in.
func TestHeadsUpKeepsTheHighestThreePriorities(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	sessions := []archive.Metadata{
		// Low cache hit: 40% over 100,000 input-side tokens, and a subagent
		// with a third of the tokens, costliest by far.
		meta("p", "claude", at, modelTokens("m", 60_000, 0, 40_000, 0)),
		meta("k", "claude", at, parentOf("p"), modelTokens("m", 50_000, 0, 0, 0)),
		meta("c", "cursor", at),
	}
	got := Compute(sessions, Options{
		Now: now, Location: newYork,
		PriceTable: PriceTable{Version: "dear", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
			{ID: "m", Family: "m", InputPerMTok: 100, OutputPerMTok: 100, CacheReadPerMTok: 100, CacheWritePerMTok: 100},
		}},
	})
	want := []NoteKind{NoteSubagentShare, NoteCostliestSession, NoteUnmeteredSessions}
	if kinds := noteKinds(got.HeadsUp); !slices.Equal(kinds, want) {
		t.Fatalf("notes = %v, want %v", kinds, want)
	}
	if len(got.HeadsUp) > MaxHeadsUp {
		t.Fatalf("%d notes, at most %d", len(got.HeadsUp), MaxHeadsUp)
	}
	// Without the Cursor session and the subagent, the cache note has room.
	got = Compute(sessions[:1], flatOptions())
	if kinds := noteKinds(got.HeadsUp); !slices.Equal(kinds, []NoteKind{NoteLowCacheHit}) {
		t.Fatalf("notes = %v, want just the cache note", kinds)
	}
}

// The window's own numbers decide: a previous period, however heavy in
// subagents or uncached, adds no note, and a window with no previous period
// has the same notes as one with.
func TestHeadsUpAreOfTheWindowNotThePreviousPeriod(t *testing.T) {
	t.Parallel()
	before := day(time.August, 20, 10)
	old := []archive.Metadata{
		meta("p", "claude", before, modelTokens("m", 100, 0, 0, 0)),
		meta("k", "claude", before, parentOf("p"), modelTokens("m", 900, 0, 0, 0)),
		meta("c", "cursor", before),
	}
	quiet := meta("now", "claude", day(time.September, 20, 10), million(1))
	for name, sessions := range map[string][]archive.Metadata{
		"only a previous period": old,
		"with a previous period": append(slices.Clone(old), quiet),
		"no previous period":     {quiet},
	} {
		if got := Compute(sessions, flatOptions()).HeadsUp; len(got) != 0 {
			t.Errorf("%s: notes = %v, want none", name, noteKinds(got))
		}
	}
}

func TestHeadsUpIsAnEmptyListInTheJSON(t *testing.T) {
	t.Parallel()
	data := mustJSON(t, Compute(nil, flatOptions()))
	if !strings.Contains(data, `"heads_up":[]`) {
		t.Fatalf("JSON = %s", data)
	}
}

// A note is aggregate data: it shares nothing with the session highlight it
// is drawn from (a renderer that edits a note must not edit the highlight),
// and carries no session id or other text of a session but its project name.
func TestHeadsUpCarriesNoSessionIdentityAndSharesNothing(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	sessions := []archive.Metadata{
		meta("sess-SECRET-1", "claude", at, project("styleprofile"), messages(2), compactions(1), million(2)),
		meta("sess-SECRET-2", "claude", at, parentOf("sess-SECRET-1"), million(2)),
		meta("sess-SECRET-3", "cursor", at),
	}
	got := Compute(sessions, flatOptions())
	costly := findNote(t, got.HeadsUp, NoteCostliestSession)
	if len(costly.Drivers) == 0 || got.Highlights.CostliestSession == nil || costly.Cost == nil || costly.Cost.USD == nil {
		t.Fatalf("costliest note = %+v, want drivers and a cost", costly)
	}
	data := mustJSON(t, got.HeadsUp)
	for _, private := range []string{"SECRET", "session_id"} {
		if strings.Contains(data, private) {
			t.Errorf("heads_up carries %q: %s", private, data)
		}
	}
	if !strings.Contains(data, "styleprofile") {
		t.Errorf("heads_up = %s, want the project name", data)
	}
	costly.Drivers[0] = "changed"
	*costly.Cost.USD = -1
	if drivers := got.Highlights.CostliestSession.Drivers; drivers[0] == "changed" {
		t.Errorf("the note's drivers are the highlight's own slice")
	}
	if usd := got.Highlights.CostliestSession.Cost.USD; usd == nil || *usd < 0 {
		t.Errorf("the note's cost is the highlight's own number")
	}
}
