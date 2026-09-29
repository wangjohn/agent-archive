package state

import (
	"os"
	"slices"
	"testing"
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
