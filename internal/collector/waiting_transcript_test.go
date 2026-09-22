package collector

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// A Cursor desktop chat is registered at its first prompt, before Cursor names
// its transcript. Until a later hook fills in the path, the registration is
// waiting: no failure, no LastError, any queued request kept, and it is
// retried (and published) once the path exists.
func TestRegistrationWithoutTranscriptPathIsWaitingNotFailed(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, "")
	reg.Harness.Name = "cursor"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	for pass := 0; pass < 2; pass++ {
		result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 0 || len(result.Published) != 0 || result.Scanned != 1 {
			t.Fatalf("pass %d result = %#v", pass, result)
		}
		status, err := local.LoadStatus()
		if err != nil || status.LastError != "" {
			t.Fatalf("pass %d status = %#v err=%v", pass, status, err)
		}
		if _, pending, err := local.loadRequest(reg.ArchiveSessionID); err != nil || !pending {
			t.Fatalf("pass %d dropped the queued request: pending=%v err=%v", pass, pending, err)
		}
	}

	// The path arrives; the next pass reads and publishes the transcript.
	reg.TranscriptPath = writeTranscript(t, t.TempDir(), reg.NativeSessionID+".jsonl", `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n"+`{"role":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
}

// The fresh-start proof also accepts a path to a file that exists but holds
// no bytes yet. Until the application writes its first record that is the
// same waiting state, one step later: no failure, no LastError, the queued
// request kept, and publication on the first pass after content arrives.
func TestRegistrationWithEmptyTranscriptIsWaitingNotFailed(t *testing.T) {
	local := newTestStore(t)
	transcript := writeTranscript(t, t.TempDir(), "native-1.jsonl", "")
	reg := registration(t, transcript)
	reg.Harness.Name = "cursor"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	for pass := 0; pass < 2; pass++ {
		result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Errors) != 0 || len(result.Published) != 0 || result.Scanned != 1 {
			t.Fatalf("pass %d result = %#v", pass, result)
		}
		status, err := local.LoadStatus()
		if err != nil || status.LastError != "" {
			t.Fatalf("pass %d status = %#v err=%v", pass, status, err)
		}
		if _, pending, err := local.loadRequest(reg.ArchiveSessionID); err != nil || !pending {
			t.Fatalf("pass %d dropped the queued request: pending=%v err=%v", pass, pending, err)
		}
		if _, _, state, found, err := local.LoadPublished(reg.ArchiveSessionID); err != nil || found {
			t.Fatalf("pass %d cached a candidate for an empty transcript: state=%v found=%v err=%v", pass, state, found, err)
		}
	}

	writeTranscript(t, filepath.Dir(transcript), "native-1.jsonl", `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n"+`{"role":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n")
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v err=%v", result, err)
	}
}
