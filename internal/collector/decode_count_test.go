package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A collector scan reads a session's published state (a whole source bundle,
// often megabytes) once, whichever way the scan goes, and hands that one
// copy to every step. Not a parallel test: the count is process-wide.
func TestScanReadsPublishedStateOnce(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n")
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	pass := func(name string, wantPublished int) {
		t.Helper()
		before := state.PublishedStateLoads()
		result, err := Run(context.Background(), local, store, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != wantPublished {
			t.Fatalf("%s: %#v %v", name, result, err)
		}
		if reads := state.PublishedStateLoads() - before; reads != 1 {
			t.Fatalf("%s: published state read %d times in one scan, want 1", name, reads)
		}
	}

	pass("first publication", 1)

	now = now.Add(time.Hour)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	pass("request on an unchanged transcript", 0)

	now = now.Add(time.Hour)
	writeTranscript(t, dir, "codex.jsonl", grownTranscript)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	pass("republication", 1)

	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	pass("metadata refresh after a parser upgrade", 1)

	now = now.Add(time.Hour)
	opts.ParserVersion = "three"
	if err := os.Remove(filepath.Clean(path)); err != nil {
		t.Fatal(err)
	}
	pass("metadata refresh without a transcript", 1)

	now = now.Add(time.Hour)
	pass("transcript missing", 0)
}
