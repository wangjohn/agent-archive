package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

// Subagents Claude Code never wrote are not a problem, so default status
// says nothing about them. --verbose counts the last week's by type, under
// the waiting ones, and --json lists them; an entry from before the week is
// left out even when no pass has pruned it yet.
func TestVerboseStatusGroupsExpiredSubagentsByType(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := local.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.WaitingSubagents = 1
	status.ExpiredSubagents = []state.ExpiredSubagent{
		{ArchiveSessionID: "stale-plan", AgentType: "Plan", ExpiredAt: now.Add(-state.ExpiredSubagentWindow - time.Hour)},
		{ArchiveSessionID: "a", ExpiredAt: now.Add(-6 * 24 * time.Hour)},
		{ArchiveSessionID: "b", AgentType: "Explore", ExpiredAt: now.Add(-5 * time.Hour)},
		{ArchiveSessionID: "c", ExpiredAt: now.Add(-4 * time.Hour)},
		{ArchiveSessionID: "d", ExpiredAt: now.Add(-3 * time.Hour)},
		{ArchiveSessionID: "e", AgentType: "Explore", ExpiredAt: now.Add(-2 * time.Hour)},
		{ArchiveSessionID: "f", ExpiredAt: now.Add(-time.Hour)},
		{ArchiveSessionID: "g", ExpiredAt: now},
	}
	if err := local.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now }

	if plain := statusOutput(t, env); strings.Contains(plain, "Subagents:") || strings.Contains(plain, "not archived in the last") || strings.Contains(plain, "2 Explore") {
		t.Fatalf("default status mentions subagents:\n%s", plain)
	}
	want := "  Subagents:     1 waiting for their transcripts\n" +
		"                 7 not archived in the last 7 days (Claude Code never wrote their transcripts; nothing to do)\n" +
		"                 5 unknown type, 2 Explore\n"
	if verbose := statusOutput(t, env, "--verbose"); !strings.Contains(verbose, want) || strings.Contains(verbose, "1 Plan") || strings.Contains(verbose, "Last error") {
		t.Fatalf("verbose status:\n%s\nwant it to contain:\n%s", verbose, want)
	}
	asJSON := statusOutput(t, env, "--json")
	if !strings.Contains(asJSON, `"expired_subagents": [`) || !strings.Contains(asJSON, `"agent_type": "Explore"`) || strings.Contains(asJSON, "stale-plan") {
		t.Fatalf("status --json:\n%s", asJSON)
	}
}

// With no subagent waiting, the expired ones take the Subagents label, and
// a tie between types lists them alphabetically.
func TestVerboseStatusShowsExpiredSubagentsAlone(t *testing.T) {
	t.Parallel()
	got := subagentDetailLines(state.Status{ExpiredSubagents: []state.ExpiredSubagent{
		{ArchiveSessionID: "a", AgentType: "general-purpose"},
		{ArchiveSessionID: "b", AgentType: "Explore"},
	}})
	want := []string{"2 not archived in the last 7 days (Claude Code never wrote their transcripts; nothing to do)", "1 Explore, 1 general-purpose"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines=%q, want %q", got, want)
	}
	if got := subagentDetailLines(state.Status{}); len(got) != 0 {
		t.Fatalf("lines with no subagents=%q", got)
	}
}
