package reader

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestMatchesCaptureCoverageAndModelEdges(t *testing.T) {
	captured := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	m := archive.Metadata{
		Harness:    archive.Harness{Name: "codex"},
		CapturedAt: captured,
		Parser:     archive.ParserInfo{Status: archive.ParserStatusComplete},
		Models: []archive.ModelSummary{
			{Attributes: map[string]string{"gen_ai.request.model": "requested"}},
			{Attributes: map[string]string{"gen_ai.response.model": "responded"}},
		},
	}
	for _, tc := range []struct {
		name   string
		filter Filter
		want   bool
	}{
		{name: "zero filter", want: true},
		{name: "all fields", filter: Filter{Harness: "codex", From: captured, To: captured, Model: "responded", RequireCompleteCoverage: true}, want: true},
		{name: "request model", filter: Filter{Model: "requested"}, want: true},
		{name: "wrong harness", filter: Filter{Harness: "claude"}},
		{name: "before lower bound", filter: Filter{From: captured.Add(time.Nanosecond)}},
		{name: "after upper bound", filter: Filter{To: captured.Add(-time.Nanosecond)}},
		{name: "missing model", filter: Filter{Model: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(m, tc.filter); got != tc.want {
				t.Fatalf("matches() = %v, want %v", got, tc.want)
			}
		})
	}
	m.CaptureGaps = []archive.CaptureGap{{Code: "missing"}}
	if matches(m, Filter{RequireCompleteCoverage: true}) || !matches(m, Filter{}) {
		t.Fatal("capture gap did not affect only complete-coverage filtering")
	}
	m.CaptureGaps = nil
	m.Parser.Status = archive.ParserStatusPartial
	if matches(m, Filter{RequireCompleteCoverage: true}) || !matches(m, Filter{}) {
		t.Fatal("partial parser did not affect only complete-coverage filtering")
	}
}

func TestMatchesSkillIdentityAndUsageEdges(t *testing.T) {
	const unknownSkillUsage SkillUsage = "unknown"
	m := archive.Metadata{
		Parser:         archive.ParserInfo{Version: "0.4.0"},
		SkillDetection: archive.SkillDetectionObservedNone,
		SkillsUsed: []archive.SkillUse{
			{Name: "review", SHA256: "used"},
		},
		SkillsAvailable: []archive.SkillSnapshot{
			{Name: "review", SHA256: "unused", Coverage: archive.SkillCoverageDiscovered},
			{Name: "review", SHA256: "installed", Coverage: archive.SkillCoverageInstalledOnly},
		},
	}
	for _, tc := range []struct {
		name   string
		filter Filter
		want   bool
	}{
		{name: "usage ignored without identity", filter: Filter{SkillUsage: SkillUsageEligibleNoUse}, want: true},
		{name: "default means used", filter: Filter{Skill: "review"}, want: true},
		{name: "unknown usage means used", filter: Filter{Skill: "review", SkillUsage: unknownSkillUsage}, want: true},
		{name: "used exact pair", filter: Filter{Skill: "review", SkillSHA256: "used", SkillUsage: SkillUsageUsed}, want: true},
		{name: "used hash-only", filter: Filter{SkillSHA256: "used"}, want: true},
		{name: "no cross-entry name/hash join", filter: Filter{Skill: "other", SkillSHA256: "used"}},
		{name: "available discovered", filter: Filter{Skill: "review", SkillSHA256: "unused", SkillUsage: SkillUsageAvailable}, want: true},
		{name: "installed is not available", filter: Filter{Skill: "review", SkillSHA256: "installed", SkillUsage: SkillUsageAvailable}},
		{name: "unused exact version", filter: Filter{Skill: "review", SkillSHA256: "unused", SkillUsage: SkillUsageEligibleNoUse}, want: true},
		{name: "used exact version", filter: Filter{Skill: "review", SkillSHA256: "used", SkillUsage: SkillUsageEligibleNoUse}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(m, tc.filter); got != tc.want {
				t.Fatalf("matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Metadata entry order is incidental; every permutation must make the same
// decision even when matching and nonmatching versions share a name.
func TestMatchesSkillEntryOrderInvariant(t *testing.T) {
	used := []archive.SkillUse{{Name: "review", SHA256: "a"}, {Name: "other", SHA256: "b"}}
	available := []archive.SkillSnapshot{
		{Name: "review", SHA256: "a", Coverage: archive.SkillCoverageInstalledOnly},
		{Name: "review", SHA256: "b", Coverage: archive.SkillCoverageEligible},
		{Name: "other", SHA256: "c", Coverage: archive.SkillCoverageDiscovered},
	}
	filters := []Filter{
		{Skill: "review", SkillSHA256: "a"},
		{Skill: "review", SkillUsage: SkillUsageAvailable},
		{Skill: "review", SkillSHA256: "b", SkillUsage: SkillUsageEligibleNoUse},
		{Skill: "review", SkillSHA256: "a", SkillUsage: SkillUsageEligibleNoUse},
		{SkillSHA256: "c", SkillUsage: SkillUsageAvailable},
	}
	baseline := make([]bool, len(filters))
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for order, indices := range orders {
		m := archive.Metadata{
			Parser:          archive.ParserInfo{Version: "0.4.0"},
			SkillDetection:  archive.SkillDetectionObservedNone,
			SkillsUsed:      []archive.SkillUse{used[order%2], used[(order+1)%2]},
			SkillsAvailable: []archive.SkillSnapshot{available[indices[0]], available[indices[1]], available[indices[2]]},
		}
		for i, f := range filters {
			got := matches(m, f)
			if order == 0 {
				baseline[i] = got
			} else if got != baseline[i] {
				t.Fatalf("order %d, filter %+v: got %v, want %v", order, f, got, baseline[i])
			}
		}
	}
}

// Replays are included by the zero filter, so every caller that does not ask
// (show by ID, retention) still sees them; list and stats ask to hide them.
func TestMatchesReplays(t *testing.T) {
	t.Parallel()
	ordinary := archive.Metadata{}
	replay := archive.Metadata{Replay: &archive.Replay{RunID: "run-1"}}
	for _, tc := range []struct {
		filter   ReplayFilter
		ordinary bool
		replays  bool
	}{
		{ReplaysIncluded, true, true},
		{ReplaysHidden, true, false},
		{ReplaysOnly, false, true},
	} {
		f := Filter{Replays: tc.filter}
		if matches(ordinary, f) != tc.ordinary || matches(replay, f) != tc.replays {
			t.Errorf("Replays %q: ordinary %t, replay %t; want %t, %t", tc.filter, matches(ordinary, f), matches(replay, f), tc.ordinary, tc.replays)
		}
	}
}
