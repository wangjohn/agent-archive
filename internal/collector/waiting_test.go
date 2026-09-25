package collector

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// With several publications held for the interval, NextReadyAt is the
// earliest due time, whatever order the sessions are scanned in.
func TestRunReportsEarliestDueTimeAmongWaitingSessions(t *testing.T) {
	for _, laterFirst := range []bool{false, true} {
		dir := t.TempDir()
		local := newTestStore(t)
		store := storagetest.NewMemoryStore()
		t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
		opts := func(at time.Time) Options {
			return Options{MachineID: "m", Now: func() time.Time { return at }, MinUploadInterval: 3 * time.Minute}
		}
		// IDs sort the scan order; laterFirst puts the later-due session first.
		earlyID, lateID := "session-a", "session-b"
		if laterFirst {
			earlyID, lateID = "session-b", "session-a"
		}
		early := registration(t, writeTranscript(t, dir, "early.jsonl", codexTranscript))
		early.ArchiveSessionID, early.NativeSessionID = earlyID, "native-"+earlyID
		late := registration(t, writeTranscript(t, dir, "late.jsonl", codexTranscript))
		late.ArchiveSessionID, late.NativeSessionID = lateID, "native-"+lateID

		// The early session publishes at t0, the late one a minute after.
		if err := local.SaveRegistration(early); err != nil {
			t.Fatal(err)
		}
		if result, err := Run(context.Background(), local, store, opts(t0)); err != nil || len(result.Published) != 1 {
			t.Fatalf("first publish: %#v %v", result, err)
		}
		if err := local.SaveRegistration(late); err != nil {
			t.Fatal(err)
		}
		if result, err := Run(context.Background(), local, store, opts(t0.Add(time.Minute))); err != nil || len(result.Published) != 1 {
			t.Fatalf("second publish: %#v %v", result, err)
		}

		more := codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`
		writeTranscript(t, dir, "early.jsonl", more)
		writeTranscript(t, dir, "late.jsonl", more)
		for _, at := range []time.Time{t0.Add(90 * time.Second), t0.Add(2 * time.Minute)} {
			result, err := Run(context.Background(), local, store, opts(at))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Waiting) != 2 || !result.NextReadyAt.Equal(t0.Add(3*time.Minute)) {
				t.Fatalf("laterFirst=%v at %v: waiting=%v next=%v, want 2 waiting and the earliest due %v", laterFirst, at, result.Waiting, result.NextReadyAt, t0.Add(3*time.Minute))
			}
		}
	}
}
