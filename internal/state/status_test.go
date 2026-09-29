package state

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// The status file keeps each problem as its own entry, beside the joined text
// older readers use, and a problem containing "; " stays one entry.
func TestStatusKeepsEachProblemAsAnEntry(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	status.SetLastErrors("2 session(s) failed to scan or publish", "list registrations: api error SlowDown: slow down; then retry")
	status.AddLastError("retention: held")
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2 session(s) failed to scan or publish", "list registrations: api error SlowDown: slow down; then retry", "retention: held"}
	if !slices.Equal(loaded.LastErrors, want) {
		t.Fatalf("LastErrors = %q want %q", loaded.LastErrors, want)
	}
	if joined := "2 session(s) failed to scan or publish; list registrations: api error SlowDown: slow down; then retry; retention: held"; loaded.LastError != joined {
		t.Fatalf("LastError = %q want %q", loaded.LastError, joined)
	}
	loaded.SetLastErrors()
	if loaded.LastError != "" || loaded.LastErrors != nil {
		t.Fatalf("no problems left %q, %q", loaded.LastError, loaded.LastErrors)
	}
	// An empty problem is no problem, so the two fields never disagree on
	// whether there is one.
	loaded.SetLastErrors("", "a", "")
	if loaded.LastError != "a" || !slices.Equal(loaded.LastErrors, []string{"a"}) {
		t.Fatalf("empty problems kept: %q, %q", loaded.LastError, loaded.LastErrors)
	}
	loaded.SetLastErrors("")
	if loaded.LastError != "" || loaded.LastErrors != nil {
		t.Fatalf("an empty problem recorded: %q, %q", loaded.LastError, loaded.LastErrors)
	}
}

// A problem added to a status file written before LastErrors existed splits
// the old joined text where status reads it split, on "; ", so adding one
// does not change how the problems already there are shown.
func TestStatusAddsToAnOlderStatusFile(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.statusPath(), []byte(`{"pending_count":0,"last_error":"a; b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.AddLastError("c")
	if want := []string{"a", "b", "c"}; !slices.Equal(status.LastErrors, want) || status.LastError != "a; b; c" {
		t.Fatalf("LastErrors = %q, LastError = %q", status.LastErrors, status.LastError)
	}
}

// The expired-subagent list is a week of history: each pass carries the
// previous entries forward, drops those a week old or more (and those a week
// or more in the future, from a clock that has since jumped back), keeps one
// entry per subagent, the latest, and keeps only the newest
// MaxExpiredSubagents.
func TestCarryExpiredSubagentsPrunesDedupesAndCaps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	previous := []ExpiredSubagent{
		{ArchiveSessionID: "week-old", ExpiredAt: now.Add(-ExpiredSubagentWindow)},
		{ArchiveSessionID: "almost-week-old", AgentType: "Plan", ExpiredAt: now.Add(-ExpiredSubagentWindow + time.Second)},
		{ArchiveSessionID: "far-future", ExpiredAt: now.Add(ExpiredSubagentWindow)},
		{ArchiveSessionID: "repeat", ExpiredAt: now.Add(-time.Hour)},
		{ArchiveSessionID: "", ExpiredAt: now},
	}
	fresh := []ExpiredSubagent{
		{ArchiveSessionID: "repeat", AgentType: "Explore", ExpiredAt: now},
		{ArchiveSessionID: "new", ExpiredAt: now},
	}
	got := CarryExpiredSubagents(previous, fresh, now)
	want := []ExpiredSubagent{
		{ArchiveSessionID: "almost-week-old", AgentType: "Plan", ExpiredAt: now.Add(-ExpiredSubagentWindow + time.Second)},
		{ArchiveSessionID: "new", ExpiredAt: now},
		{ArchiveSessionID: "repeat", AgentType: "Explore", ExpiredAt: now},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("carried\n%+v\nwant\n%+v", got, want)
	}
	if got := CarryExpiredSubagents(nil, nil, now); got != nil {
		t.Fatalf("empty list = %#v, want nil so status.json omits it", got)
	}

	var many []ExpiredSubagent
	for i := range MaxExpiredSubagents + 5 {
		many = append(many, ExpiredSubagent{ArchiveSessionID: fmt.Sprintf("s%03d", i), ExpiredAt: now.Add(time.Duration(i-200) * time.Minute)})
	}
	capped := CarryExpiredSubagents(many[:50], many[50:], now)
	if len(capped) != MaxExpiredSubagents || capped[0].ArchiveSessionID != "s005" || capped[len(capped)-1].ArchiveSessionID != fmt.Sprintf("s%03d", MaxExpiredSubagents+4) {
		t.Fatalf("capped to %d, first %+v, last %+v; want the newest %d", len(capped), capped[0], capped[len(capped)-1], MaxExpiredSubagents)
	}
}

// ReplaceLastError swaps one recorded problem in place, or adds the new one
// when the old is not recorded.
func TestStatusReplacesOneProblem(t *testing.T) {
	t.Parallel()
	var status Status
	status.SetLastErrors("a", "b", "c")
	status.ReplaceLastError("b", "B")
	if want := []string{"a", "B", "c"}; !slices.Equal(status.LastErrors, want) || status.LastError != "a; B; c" {
		t.Fatalf("LastErrors = %q, LastError = %q", status.LastErrors, status.LastError)
	}
	status.ReplaceLastError("missing", "d")
	if want := []string{"a", "B", "c", "d"}; !slices.Equal(status.LastErrors, want) {
		t.Fatalf("LastErrors = %q want %q", status.LastErrors, want)
	}
}
